package main

import (
	"reflect"
	"testing"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/appconfig"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/compose"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/dockerfile"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// Generated stubs are copied into every service, so declaring a client or
// server form proves nothing; only an edge out of hand-written code does.
func TestDeriveTopology_RolesComeFromHandWrittenEdges(t *testing.T) {
	gen := func(svc string) []parser.NodeInfo {
		f := "src/" + svc + "/genproto/demo_grpc.pb.go"
		return []parser.NodeInfo{
			{Kind: "File", FilePath: f, Language: "go"},
			{Kind: "Func", Name: "NewCartServiceClient", FilePath: f, Language: "go"},
			{Kind: "Func", Name: "RegisterCartServiceServer", FilePath: f, Language: "go"},
		}
	}
	nodes := append(gen("cart"), gen("checkout")...)
	nodes = append(nodes,
		parser.NodeInfo{Kind: "File", FilePath: "src/cart/main.go", Language: "go"},
		parser.NodeInfo{Kind: "Func", Name: "main", FilePath: "src/cart/main.go", Language: "go"},
		parser.NodeInfo{Kind: "File", FilePath: "src/checkout/main.go", Language: "go"},
		parser.NodeInfo{Kind: "Func", Name: "main", FilePath: "src/checkout/main.go", Language: "go"},
		// a third service copies the stubs and uses neither form
		parser.NodeInfo{Kind: "File", FilePath: "src/email/app.rb", Language: "ruby"},
	)
	edges := []parser.EdgeInfo{
		{Kind: "calls", Source: "src/cart/main.go::main", Target: "src/cart/genproto/demo_grpc.pb.go::RegisterCartServiceServer", File: "src/cart/main.go"},
		{Kind: "calls", Source: "src/checkout/main.go::main", Target: "src/checkout/genproto/demo_grpc.pb.go::NewCartServiceClient", File: "src/checkout/main.go"},
	}
	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})
	top := deriveTopology(g)

	if top.container != "src" {
		t.Fatalf("container = %q, want src", top.container)
	}
	got := map[string][]string{}
	for name, s := range top.services {
		got[name] = sortedKeys(s.calls)
	}
	want := map[string][]string{"cart": {}, "checkout": {"cart"}, "email": {}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

// frontend and its Cypress image frontendTests both build src/frontend. The
// component is the deployment others address; handing it to whichever name
// sorted last gave frontend's gRPC calls to the test harness.
func TestDeriveTopology_ComponentGoesToTheAddressedBuilder(t *testing.T) {
	src := []byte(`services:
  frontend:
    build:
      dockerfile: ./src/frontend/Dockerfile
  frontendTests:
    build:
      dockerfile: ./src/frontend/Dockerfile.cypress
    environment:
      - CYPRESS_baseUrl=http://frontend:8080
  cart:
    build:
      dockerfile: ./src/cart/Dockerfile
  frontend-proxy:
    environment:
      - FRONTEND_HOST=frontend
`)
	res := compose.Parse("compose.yaml", src)
	nodes := append(res.Nodes,
		parser.NodeInfo{Kind: "File", FilePath: "src/frontend/main.go", Language: "go"},
		parser.NodeInfo{Kind: "Func", Name: "main", FilePath: "src/frontend/main.go", Language: "go"},
		parser.NodeInfo{Kind: "File", FilePath: "src/frontend/genproto/demo_grpc.pb.go", Language: "go"},
		parser.NodeInfo{Kind: "Func", Name: "NewCartServiceClient", FilePath: "src/frontend/genproto/demo_grpc.pb.go", Language: "go"},
		parser.NodeInfo{Kind: "Func", Name: "RegisterCartServiceServer", FilePath: "src/frontend/genproto/demo_grpc.pb.go", Language: "go"},
		parser.NodeInfo{Kind: "File", FilePath: "src/cart/main.go", Language: "go"},
		parser.NodeInfo{Kind: "Func", Name: "main", FilePath: "src/cart/main.go", Language: "go"},
		parser.NodeInfo{Kind: "File", FilePath: "src/cart/genproto/demo_grpc.pb.go", Language: "go"},
		parser.NodeInfo{Kind: "Func", Name: "NewCartServiceClient", FilePath: "src/cart/genproto/demo_grpc.pb.go", Language: "go"},
		parser.NodeInfo{Kind: "Func", Name: "RegisterCartServiceServer", FilePath: "src/cart/genproto/demo_grpc.pb.go", Language: "go"},
	)
	edges := append(res.Edges,
		parser.EdgeInfo{Kind: "calls", Source: "src/frontend/main.go::main", Target: "src/frontend/genproto/demo_grpc.pb.go::NewCartServiceClient", File: "src/frontend/main.go"},
		parser.EdgeInfo{Kind: "calls", Source: "src/cart/main.go::main", Target: "src/cart/genproto/demo_grpc.pb.go::RegisterCartServiceServer", File: "src/cart/main.go"},
	)
	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})
	top := deriveTopology(g)

	got := map[string][]string{}
	for name, s := range top.services {
		got[name] = sortedKeys(s.calls)
	}
	want := map[string][]string{"cart": {}, "frontend": {"cart"}, "frontend-proxy": {"frontend"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v (frontendTests is a variant of frontend)", got, want)
	}
}

// A Gradle monorepo: images say what each service runs, config says where each
// client goes and what base path each service serves, and type references say
// which clients the service's code reaches.
//
//   - svc-a's BClient url expands to a path svc-b serves: a call to svc-b, and
//     b-service becomes svc-b's alias;
//   - svc-a's ExtApi and the shared library's ExtSharedClient name one address
//     under two names: one external service, the second name its alias;
//   - the library client counts because svc-a's code reaches it through
//     Uploader; svc-a's UnusedClient is referenced by nothing and is no call;
//   - shared is copied into svc-a's image and has none of its own: a library,
//     not a service.
func TestDeriveTopology_ClientsResolveThroughConfigImagesAndReach(t *testing.T) {
	const (
		aDir = "svc-a/src/main/java/a/"
		sDir = "shared/src/main/java/s/"
		bDir = "svc-b/src/main/java/b/"
	)
	var nodes []parser.NodeInfo
	var edges []parser.EdgeInfo
	add := func(r parser.Result) {
		nodes = append(nodes, r.Nodes...)
		edges = append(edges, r.Edges...)
	}
	add(dockerfile.Parse("svc-a/Dockerfile", []byte("FROM x\nCOPY svc-a svc-a\nCOPY shared shared\n")))
	add(dockerfile.Parse("svc-b/Dockerfile", []byte("FROM x\nCOPY svc-b svc-b\n")))
	add(appconfig.Parse("svc-a/src/main/resources/application-dev.yml", []byte(`b:
  url : http://10.0.0.1:8100/api/b
ext:
  url: https://ext.example.com/v2
`)))
	add(appconfig.Parse("svc-b/src/main/resources/application.yml", []byte("server:\n  servlet:\n    context-path: /api/b\n")))
	class := func(file, name string) {
		nodes = append(nodes,
			parser.NodeInfo{Kind: "File", FilePath: file, Language: "java"},
			parser.NodeInfo{Kind: "Class", Name: name, FilePath: file, Language: "java"})
	}
	feign := func(file, name, clientName, url string) {
		class(file, name)
		edges = append(edges, parser.EdgeInfo{Kind: "references", Source: file + "::" + name, Target: "FeignClient", File: file,
			Extra: map[string]string{parser.ExtraAnnotation: "FeignClient", "arg.name": clientName, "arg.url": url}})
	}
	ref := func(file, from, to string) {
		edges = append(edges, parser.EdgeInfo{Kind: "references", Source: file + "::" + from, Target: to, File: file,
			Extra: map[string]string{"callName": to, "typeName": to}})
	}
	class(aDir+"AService.java", "AService")
	feign(aDir+"BClient.java", "BClient", "b-service", "${b.url}")
	feign(aDir+"ExtApi.java", "ExtApi", "ext", "${ext.url}")
	feign(aDir+"UnusedClient.java", "UnusedClient", "zzz", "http://nowhere/")
	class(sDir+"Uploader.java", "Uploader")
	feign(sDir+"ExtSharedClient.java", "ExtSharedClient", "ext-shared", "${ext.url}")
	class(bDir+"BController.java", "BController")
	ref(aDir+"AService.java", "AService", "BClient")
	ref(aDir+"AService.java", "AService", "ExtApi")
	ref(aDir+"AService.java", "AService", "Uploader")
	edges = append(edges, parser.EdgeInfo{Kind: "imports", Source: aDir + "AService.java", Target: "s.Uploader"})
	ref(sDir+"Uploader.java", "Uploader", "ExtSharedClient")

	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})
	top := deriveTopology(g)

	got := map[string][]string{}
	aliases := map[string][]string{}
	for name, s := range top.services {
		got[name] = sortedKeys(s.calls)
		aliases[name] = sortedKeys(s.aliases)
	}
	want := map[string][]string{"svc-a": {"ext", "svc-b"}, "svc-b": {}, "ext": {}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v\n%s", got, want, top.stats())
	}
	wantAliases := map[string][]string{"svc-a": {}, "svc-b": {"b-service"}, "ext": {"ext-shared"}}
	if !reflect.DeepEqual(aliases, wantAliases) {
		t.Fatalf("aliases = %v, want %v", aliases, wantAliases)
	}
	if !top.libraries["shared"] || !top.services["ext"].external {
		t.Fatalf("libraries = %v, ext external = %v", top.libraries, top.services["ext"].external)
	}

	doc := top.triage(2, 3, "")
	byName := map[string]triageService{}
	for _, s := range doc.Services {
		byName[s.Name] = s
	}
	for _, s := range doc.Services {
		for _, c := range s.Calls {
			if _, ok := byName[c]; !ok {
				t.Fatalf("%s calls %s, which is not an entry", s.Name, c)
			}
			found := false
			for _, cb := range byName[c].CalledBy {
				found = found || cb == s.Name
			}
			if !found {
				t.Fatalf("%s.called_by misses %s", c, s.Name)
			}
		}
		if s.Team == "" || s.Tier < 1 || s.Tier > 4 {
			t.Fatalf("%s: team %q tier %d would fail lint", s.Name, s.Team, s.Tier)
		}
	}
	if byName["ext"].Domain != "external" || byName["ext"].Tier != 3 {
		t.Fatalf("ext = %+v, want an external tier-3 entry", byName["ext"])
	}
}

// An imperative call (RestTemplate, WebServiceTemplate) resolves the same way
// a declared client does when the address is already sitting at the call:
// java_calls.go resolves a literal or a field's Spring @Value(...) at parse
// time into ExtraArgPrefix+"url", so the service map reads it exactly like a
// Feign client's url.
func TestImperativeClients_ResolvesAFixedAddressLikeADeclaredClient(t *testing.T) {
	const dir = "gizmo/src/main/java/g/"
	var nodes []parser.NodeInfo
	var edges []parser.EdgeInfo
	add := func(r parser.Result) {
		nodes = append(nodes, r.Nodes...)
		edges = append(edges, r.Edges...)
	}
	add(dockerfile.Parse("gizmo/Dockerfile", []byte("FROM x\nCOPY gizmo gizmo\n")))
	add(appconfig.Parse("gizmo/src/main/resources/application.yml", []byte("ext:\n  url: https://ext.example.com/v2\n")))

	nodes = append(nodes,
		parser.NodeInfo{Kind: "File", FilePath: dir + "GizmoService.java", Language: "java"},
		parser.NodeInfo{Kind: "Func", Name: "call", ParentName: "GizmoService", FilePath: dir + "GizmoService.java", Language: "java"},
	)
	edges = append(edges, parser.EdgeInfo{
		Kind: parser.EdgeCalls, Source: dir + "GizmoService.java::GizmoService.call",
		Target: "getForObject", File: dir + "GizmoService.java", Line: 10,
		Extra: map[string]string{
			parser.ExtraJavaRecv: "1", "recvType": "RestTemplate", "callName": "getForObject",
			parser.ExtraArgPrefix + "url": "${ext.url}",
		},
	})

	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})
	top := deriveTopology(g)

	svc := top.services["gizmo"]
	if svc == nil {
		t.Fatalf("services = %v, want gizmo", sortedKeys(top.services))
	}
	if _, ok := svc.calls["ext.example.com"]; !ok {
		t.Fatalf("gizmo.calls = %v, want ext.example.com\n%s", sortedKeys(svc.calls), top.stats())
	}
	if ext := top.services["ext.example.com"]; ext == nil || !ext.external {
		t.Fatalf("ext.example.com = %+v, want an external entry", ext)
	}
}

// A hand-written HTTP client is usually an interface plus one impl whose
// method just forwards what it was given (a RestTemplateService.post(url,
// ...) calling restTemplate.postForEntity(url, ...)) — the shape
// RestTemplateServiceImpl/ExternalTokenServiceImpl takes in the real repo
// this was built for. Resolution must walk back through the impl's callers,
// through its interface, to the one caller passing a config-shaped value.
func TestImperativeClients_ResolvesThroughInterfaceAndOneCaller(t *testing.T) {
	const dir = "widget/src/main/java/w/"
	var nodes []parser.NodeInfo
	var edges []parser.EdgeInfo
	add := func(r parser.Result) {
		nodes = append(nodes, r.Nodes...)
		edges = append(edges, r.Edges...)
	}
	add(dockerfile.Parse("widget/Dockerfile", []byte("FROM x\nCOPY widget widget\n")))
	add(appconfig.Parse("widget/src/main/resources/application.yml", []byte("ext:\n  url: https://ext.example.com/v2\n")))

	nodes = append(nodes,
		// interface Sender { post(); } — declared, no body.
		parser.NodeInfo{Kind: "File", FilePath: dir + "Sender.java", Language: "java"},
		parser.NodeInfo{Kind: "Class", Name: "Sender", FilePath: dir + "Sender.java", Language: "java"},
		parser.NodeInfo{Kind: "Func", Name: "post", ParentName: "Sender", FilePath: dir + "Sender.java", Language: "java"},
		// class SenderImpl implements Sender { post() { restTemplate.postForEntity(url, ...); } }
		parser.NodeInfo{Kind: "File", FilePath: dir + "SenderImpl.java", Language: "java"},
		parser.NodeInfo{Kind: "Class", Name: "SenderImpl", FilePath: dir + "SenderImpl.java", Language: "java"},
		parser.NodeInfo{Kind: "Func", Name: "post", ParentName: "SenderImpl", FilePath: dir + "SenderImpl.java", Language: "java"},
		// class Caller { send() { sender.post(extUrl); } }
		parser.NodeInfo{Kind: "File", FilePath: dir + "Caller.java", Language: "java"},
		parser.NodeInfo{Kind: "Class", Name: "Caller", FilePath: dir + "Caller.java", Language: "java"},
		parser.NodeInfo{Kind: "Func", Name: "send", ParentName: "Caller", FilePath: dir + "Caller.java", Language: "java"},
	)
	edges = append(edges,
		parser.EdgeInfo{Kind: parser.EdgeReferences, Source: dir + "SenderImpl.java::SenderImpl", Target: "Sender",
			File: dir + "SenderImpl.java", Extra: map[string]string{"callName": "Sender", "typeName": "Sender"}},
		parser.EdgeInfo{Kind: parser.EdgeCalls, Source: dir + "SenderImpl.java::SenderImpl.post", Target: "postForEntity",
			File: dir + "SenderImpl.java", Line: 5,
			Extra: map[string]string{parser.ExtraJavaRecv: "1", "recvType": "RestTemplate", "callName": "postForEntity",
				parser.ExtraArgURLParam0: "1"}},
		parser.EdgeInfo{Kind: parser.EdgeCalls, Source: dir + "Caller.java::Caller.send", Target: "post",
			File: dir + "Caller.java", Line: 8,
			Extra: map[string]string{parser.ExtraJavaRecv: "1", "recvType": "Sender", "callName": "post",
				parser.ExtraArgPrefix + "url": "${ext.url}"}},
	)

	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})
	top := deriveTopology(g)

	svc := top.services["widget"]
	if svc == nil {
		t.Fatalf("services = %v, want widget", sortedKeys(top.services))
	}
	if _, ok := svc.calls["ext.example.com"]; !ok {
		t.Fatalf("widget.calls = %v, want ext.example.com\n%s", sortedKeys(svc.calls), top.stats())
	}
}

// Test code exercises a client; it does not become the service issuing the
// call. A TestRestTemplate call inside src/test/java must produce nothing.
func TestImperativeClients_TestCodeNeverProducesACall(t *testing.T) {
	const dir = "gizmo/src/test/java/g/"
	var nodes []parser.NodeInfo
	var edges []parser.EdgeInfo
	add := func(r parser.Result) {
		nodes = append(nodes, r.Nodes...)
		edges = append(edges, r.Edges...)
	}
	add(dockerfile.Parse("gizmo/Dockerfile", []byte("FROM x\nCOPY gizmo gizmo\n")))
	add(appconfig.Parse("gizmo/src/main/resources/application.yml", []byte("ext:\n  url: https://ext.example.com/v2\n")))

	nodes = append(nodes,
		parser.NodeInfo{Kind: "File", FilePath: dir + "GizmoServiceTest.java", Language: "java"},
		parser.NodeInfo{Kind: "Func", Name: "shouldCallExt", ParentName: "GizmoServiceTest", FilePath: dir + "GizmoServiceTest.java", Language: "java"},
	)
	edges = append(edges, parser.EdgeInfo{
		Kind: parser.EdgeCalls, Source: dir + "GizmoServiceTest.java::GizmoServiceTest.shouldCallExt",
		Target: "getForObject", File: dir + "GizmoServiceTest.java", Line: 10,
		Extra: map[string]string{
			parser.ExtraJavaRecv: "1", "recvType": "TestRestTemplate", "callName": "getForObject",
			parser.ExtraArgPrefix + "url": "${ext.url}",
		},
	})

	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})
	top := deriveTopology(g)

	if svc := top.services["gizmo"]; svc != nil && len(svc.calls) != 0 {
		t.Fatalf("gizmo.calls = %v, want none (the only call site is src/test)", sortedKeys(svc.calls))
	}
	if _, ok := top.services["ext.example.com"]; ok {
		t.Fatalf("ext.example.com should not exist: its only caller is test code")
	}
}

// An alias must never equal another identity's own name: a service whose
// base path collides with another real service's own name must not alias it.
func TestDeriveTopology_AliasNeverEqualsAnotherIdentity(t *testing.T) {
	var nodes []parser.NodeInfo
	var edges []parser.EdgeInfo
	add := func(r parser.Result) {
		nodes = append(nodes, r.Nodes...)
		edges = append(edges, r.Edges...)
	}
	add(dockerfile.Parse("host-api/Dockerfile", []byte("FROM x\nCOPY host-api host-api\n")))
	add(dockerfile.Parse("real-service/Dockerfile", []byte("FROM x\nCOPY real-service real-service\n")))
	// host-api serves the base path /real-service — the same string as the
	// name of a different, real service in this repo.
	add(appconfig.Parse("host-api/src/main/resources/application.yml", []byte("server:\n  servlet:\n    context-path: /real-service\n")))
	add(appconfig.Parse("caller/src/main/resources/application.yml", []byte("h:\n  url: http://x/real-service/ping\n")))
	add(dockerfile.Parse("caller/Dockerfile", []byte("FROM x\nCOPY caller caller\n")))

	class := func(file, name string) {
		nodes = append(nodes,
			parser.NodeInfo{Kind: "File", FilePath: file, Language: "java"},
			parser.NodeInfo{Kind: "Class", Name: name, FilePath: file, Language: "java"})
	}
	class("real-service/src/main/java/r/Marker.java", "Marker")
	class("host-api/src/main/java/h/Marker.java", "Marker")
	feignFile := "caller/src/main/java/c/Client.java"
	class(feignFile, "Client")
	edges = append(edges, parser.EdgeInfo{Kind: "references", Source: feignFile + "::Client", Target: "FeignClient", File: feignFile,
		Extra: map[string]string{parser.ExtraAnnotation: "FeignClient", "arg.name": "real-service", "arg.url": "${h.url}"}})
	edges = append(edges, parser.EdgeInfo{Kind: "references", Source: "caller/src/main/java/c/Caller.java::Caller", Target: "Client", File: "caller/src/main/java/c/Caller.java",
		Extra: map[string]string{"callName": "Client", "typeName": "Client"}})
	class("caller/src/main/java/c/Caller.java", "Caller")

	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})
	top := deriveTopology(g)

	if top.services["real-service"] == nil {
		t.Fatalf("services = %v, want real-service to still exist", sortedKeys(top.services))
	}
	if top.services["host-api"].aliases["real-service"] {
		t.Fatalf("host-api.aliases = %v, must not alias real-service's own name", sortedKeys(top.services["host-api"].aliases))
	}
}
