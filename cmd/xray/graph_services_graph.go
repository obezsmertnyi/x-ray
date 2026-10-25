package main

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// The service map is derived from the linked code graph alone — the nodes the
// extractors emitted and the edges graph.Build linked — so every call it
// reports rests on an edge get_graph_node shows. Where it misses one, the fix
// belongs in an extractor or the linker, which is what makes the eval (-truth)
// a measure of the graph rather than of this file. There is one derivation for
// every stack: a Spring monorepo and a polyglot compose demo differ in which
// facts their graphs hold, not in which rules read them.
//
//  1. Components. The container is the directory scoring highest on child
//     directories holding code times the share of all code files beneath it
//     (src/ in the OpenTelemetry demo, the root of a Gradle monorepo); each
//     such child is one component. Child count alone picked
//     src/frontend/components, a UI folder with more subfolders than src/.
//  2. What runs. An Image (a Dockerfile) runs the components it copies: its
//     own, and the shared modules a Gradle service image copies in beside it,
//     which are its libraries. A compose service is a deployment, tied to a
//     component by the Dockerfile its build names. The identities are the
//     deployments when there are any, else the components with an image, else
//     every component.
//  3. Endpoints. What a service answers to: its deployment name (a host), the
//     base path its config serves under (serverPathKeys) and the name it
//     registers as (serviceNameKeys), all Config nodes.
//  4. Calls, each an address resolved to an endpoint:
//     - gRPC: generators emit a client and a server form of every service in
//     one file (NewCartServiceClient + RegisterCartServiceServer); an edge
//     from hand-written code to a client form calls the one component whose
//     code reaches the server form;
//     - declared HTTP clients (httpClientAnnotations, e.g. @FeignClient) in
//     code the service runs and reaches: the url argument expanded through
//     Config nodes names a host or a served base path; with no url, the name
//     is a registered service name. An address naming no service is a service
//     outside the repo, one per address however many client names it has;
//     - environment: a key a deployment passes, expanded through Config nodes,
//     whose value names another deployment as a host.

var (
	rpcClientForms = []*regexp.Regexp{
		regexp.MustCompile(`^New(\w+)Client$`),                 // Go
		regexp.MustCompile(`^(\w+?)(?:Blocking|Future)?Stub$`), // Python, Java
		regexp.MustCompile(`^(\w+)Client$`),                    // C#, TypeScript, Rust
	}
	rpcServerForms = []*regexp.Regexp{
		regexp.MustCompile(`^Register(\w+)Server$`),         // Go
		regexp.MustCompile(`^add_(\w+)Servicer_to_server$`), // Python
		regexp.MustCompile(`^(\w+)Servicer$`),               // Python base class
		regexp.MustCompile(`^(\w+)ImplBase$`),               // Java
		regexp.MustCompile(`^(\w+)Base$`),                   // C#
		regexp.MustCompile(`^(\w+)Server$`),                 // Go interface, TypeScript, Rust
	}
)

// Framework vocabulary: the names under which a framework declares what the
// derivation reads. Like the gRPC forms above, these are the frameworks' own
// spellings, and a new framework is a new row, not a new rule.
var (
	// httpClientAnnotations maps a declarative HTTP client annotation to its
	// address argument and its name arguments (first present wins).
	httpClientAnnotations = map[string]struct {
		url  string
		name []string
	}{
		"FeignClient":        {url: "url", name: []string{"name", "value"}},     // Spring Cloud OpenFeign
		"RegisterRestClient": {url: "baseUri", name: []string{"configKey"}},     // MicroProfile / Quarkus
		"HttpExchange":       {url: "url", name: []string{"value"}},             // Spring 6 HTTP interface
		"Client":             {url: "value", name: []string{"id", "serviceId"}}, // Micronaut
	}
	// queueListenerAnnotations maps a listener annotation to its destination
	// argument.
	queueListenerAnnotations = map[string]string{
		"JmsListener":    "destination",
		"KafkaListener":  "topics",
		"RabbitListener": "queues",
		"SqsListener":    "value",
	}
	// serverPathKeys are the config keys a service's base path is served under.
	serverPathKeys = map[string]bool{
		"server.servlet.context-path":   true, // Spring Boot 2+
		"server.context-path":           true, // Spring Boot 1
		"spring.webflux.base-path":      true,
		"quarkus.http.root-path":        true,
		"micronaut.server.context-path": true,
	}
	// serviceNameKeys are the config keys a service registers its name under.
	serviceNameKeys = map[string]bool{
		"spring.application.name":    true,
		"quarkus.application.name":   true,
		"micronaut.application.name": true,
	}
	// imperativeHTTPCallNames are imperative HTTP/SOAP client calls whose
	// first argument is the address — RestTemplate, WebClient and Spring-WS's
	// WebServiceTemplate/WebServiceGatewaySupport — distinctive enough to read
	// wherever they are called, the same way a gRPC stub form is: no framework
	// but these names this one.
	imperativeHTTPCallNames = map[string]bool{
		"getForObject": true, "getForEntity": true,
		"postForObject": true, "postForEntity": true, "postForLocation": true,
		"patchForObject": true, "headForHeaders": true, "optionsForAllow": true,
		"marshalSendAndReceive": true, "marshalSendAndReceiveToResult": true,
		"sendSourceAndReceiveToResult": true,
	}
	// genericHTTPCallNames repeat common unrelated APIs (Map.put,
	// java.io.File.delete, a JPA repository) and are read only when the
	// receiver's declared type confirms an HTTP client (httpClientTypes).
	genericHTTPCallNames = map[string]bool{
		"put": true, "delete": true, "exchange": true, "execute": true,
		"sendAndReceive": true, "baseUrl": true,
	}
	// httpClientTypes are the declared receiver types genericHTTPCallNames
	// must be seen on.
	httpClientTypes = map[string]bool{
		"RestTemplate": true, "TestRestTemplate": true, "AsyncRestTemplate": true,
		"WebClient": true, "WebServiceTemplate": true, "WebServiceOperations": true,
		"WebServiceGatewaySupport": true,
	}
)

// lastSegment is the symbol an edge end names: the part after "::", the last
// dotted name, with any repo// prefix dropped.
func lastSegment(qn string) string {
	if i := strings.LastIndex(qn, "::"); i >= 0 {
		qn = qn[i+2:]
	}
	if i := strings.LastIndex(qn, "//"); i >= 0 {
		qn = qn[i+2:]
	}
	if i := strings.LastIndexAny(qn, ".:"); i >= 0 {
		qn = qn[i+1:]
	}
	return qn
}

// stripRepo drops a qn's repo// prefix.
func stripRepo(qn string) string {
	if _, after, ok := strings.Cut(qn, "//"); ok {
		return after
	}
	return qn
}

func rpcForm(name string, forms []*regexp.Regexp) string {
	for _, re := range forms {
		if m := re.FindStringSubmatch(name); m != nil {
			return m[1]
		}
	}
	return ""
}

type component struct {
	name, root string
	clientOf   map[string][]string // interface -> evidence
	serverOf   map[string][]string
}

type topoService struct {
	name       string
	identity   string          // what the name comes from, for the description
	component  string          // "" for a deployment built from no component
	runs       map[string]bool // components whose code the service runs
	clientOf   map[string][]string
	serverOf   map[string][]string
	calls      map[string][]string // service -> evidence
	aliases    map[string]bool
	paths      []string        // base paths it serves
	registered map[string]bool // names it registers under (spring.application.name)
	consumes   map[string][]string
	external   bool
}

type topology struct {
	container      string
	components     map[string]*component
	services       map[string]*topoService
	libraries      map[string]bool
	interfaces     []string
	generated      map[string]bool
	unserved       map[string]bool // interfaces a client uses that no component serves
	ambiguous      map[string][]string
	images         int
	deployments    int
	clients        int
	unusedClients  int
	externals      int
	telemetrySinks []string
}

func newService(name, identity string) *topoService {
	return &topoService{name: name, identity: identity, runs: map[string]bool{},
		clientOf: map[string][]string{}, serverOf: map[string][]string{}, calls: map[string][]string{},
		aliases: map[string]bool{}, consumes: map[string][]string{}, registered: map[string]bool{}}
}

func deriveTopology(g *graph.Graph) *topology {
	nodes := g.Nodes()
	t := &topology{components: map[string]*component{}, services: map[string]*topoService{},
		libraries: map[string]bool{}, generated: map[string]bool{},
		unserved: map[string]bool{}, ambiguous: map[string][]string{}}

	// 1. Components.
	children := map[string]map[string]bool{} // dir -> child dirs holding code below them
	filesBelow := map[string]int{}
	totalFiles := 0
	for _, n := range nodes {
		if n.Kind != "File" {
			continue
		}
		totalFiles++
		for d := path.Dir(n.FilePath); ; d = path.Dir(d) {
			filesBelow[d]++
			parent := path.Dir(d)
			if d == "." {
				break
			}
			if children[parent] == nil {
				children[parent] = map[string]bool{}
			}
			children[parent][d] = true
		}
	}
	best := -1.0
	for _, dir := range sortedKeys(children) {
		score := float64(len(children[dir])) * float64(filesBelow[dir]) / float64(max(totalFiles, 1))
		if dir == "." {
			score = float64(len(children[dir]))
		}
		if score > best {
			best, t.container = score, dir
		}
	}
	for kid := range children[t.container] {
		name := path.Base(kid)
		t.components[name] = &component{name: name, root: kid,
			clientOf: map[string][]string{}, serverOf: map[string][]string{}}
	}

	// 2. gRPC interfaces: declared in both forms in one file.
	declared := map[string]map[string][2]bool{} // file -> interface -> {client, server}
	for _, n := range nodes {
		if n.Kind == "File" {
			continue
		}
		name := lastSegment(n.QN)
		c, s := rpcForm(name, rpcClientForms), rpcForm(name, rpcServerForms)
		for _, x := range []struct {
			iface  string
			server bool
		}{{c, false}, {s, true}} {
			if x.iface == "" {
				continue
			}
			if declared[n.FilePath] == nil {
				declared[n.FilePath] = map[string][2]bool{}
			}
			f := declared[n.FilePath][x.iface]
			if x.server {
				f[1] = true
			} else {
				f[0] = true
			}
			declared[n.FilePath][x.iface] = f
		}
	}
	ifaces := map[string]bool{}
	for file, m := range declared {
		for iface, f := range m {
			if f[0] && f[1] {
				ifaces[iface] = true
				t.generated[file] = true
			}
		}
	}
	t.interfaces = sortedKeys(ifaces)
	for _, n := range nodes {
		if t.generated[n.FilePath] {
			continue
		}
		c := t.components[t.componentOf(n.FilePath)]
		if c == nil {
			continue
		}
		for _, e := range g.Callees(n.QN, "") {
			if e.Kind == parser.EdgeContains {
				continue
			}
			target := lastSegment(e.Target)
			ev := fmt.Sprintf("%s:%d %s %s", e.File, e.Line, e.Kind, target)
			if iface := rpcForm(target, rpcServerForms); ifaces[iface] {
				c.serverOf[iface] = append(c.serverOf[iface], ev)
			} else if iface := rpcForm(target, rpcClientForms); ifaces[iface] {
				c.clientOf[iface] = append(c.clientOf[iface], ev)
			}
		}
	}

	cfg := newConfigIndex(nodes, t)
	t.identities(nodes, g, cfg)
	t.endpoints(cfg)
	t.envCalls(nodes, g, cfg)
	t.clientCalls(nodes, g, cfg)
	t.dropVariants()
	t.grpcCalls()
	return t
}

// componentOf is the component a repo-relative file belongs to, or "".
func (t *topology) componentOf(file string) string {
	rel := file
	if t.container != "." {
		if !strings.HasPrefix(file, t.container+"/") {
			return ""
		}
		rel = strings.TrimPrefix(file, t.container+"/")
	}
	name, _, ok := strings.Cut(rel, "/")
	if !ok || t.components[name] == nil {
		return ""
	}
	return name
}

// componentAt is the component whose root is dir or holds it, or "".
func (t *topology) componentAt(dir string) string {
	dir = path.Clean(strings.TrimPrefix(dir, "./"))
	if dir == "." || dir == "/" {
		return ""
	}
	return t.componentOf(dir + "/x")
}

// configIndex is every Config node's value by key, and by component, so a
// service reads the keys of the code it runs before the repo-wide ones (.env).
type configIndex struct {
	global map[string][]string
	byComp map[string]map[string][]string
	// dotenv holds .env keys, first file then overrides, as compose applies them.
	dotenv map[string]string
}

func newConfigIndex(nodes []*graph.Node, t *topology) *configIndex {
	ci := &configIndex{global: map[string][]string{}, byComp: map[string]map[string][]string{}, dotenv: map[string]string{}}
	add := func(m map[string][]string, k, v string) {
		for _, x := range m[k] {
			if x == v {
				return
			}
		}
		m[k] = append(m[k], v)
	}
	for _, pass := range []bool{false, true} {
		for _, n := range nodes {
			if n.Kind != "Config" {
				continue
			}
			if n.Language == "dotenv" {
				if (path.Base(n.FilePath) != ".env") == pass {
					ci.dotenv[n.Name] = n.Body
				}
				continue
			}
			if pass {
				continue
			}
			c := t.componentOf(n.FilePath)
			if c == "" {
				add(ci.global, n.Name, n.Body)
				continue
			}
			if ci.byComp[c] == nil {
				ci.byComp[c] = map[string][]string{}
			}
			add(ci.byComp[c], n.Name, n.Body)
		}
	}
	return ci
}

// values is what key holds for a service running comps: their own config,
// else the repo-wide config, else .env.
func (ci *configIndex) values(key string, comps map[string]bool) []string {
	var out []string
	for _, c := range sortedKeys(comps) {
		out = append(out, ci.byComp[c][key]...)
	}
	if len(out) == 0 {
		out = ci.global[key]
	}
	if len(out) == 0 {
		if v, ok := ci.dotenv[key]; ok {
			out = []string{v}
		}
	}
	return uniqSorted(out)
}

// reVarRef is a placeholder: ${KEY}, ${KEY:-default} (shell, compose),
// ${key.name:default} (Spring), $KEY.
var reVarRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_.\-\[\]]*)(?::-?([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// expand resolves placeholders through config, every value a key holds in any
// profile giving its own result (bounded), so an address differing per
// environment is matched in each.
func (ci *configIndex) expand(v string, comps map[string]bool) []string {
	out := []string{v}
	for depth := 0; depth < 5; depth++ {
		var next []string
		changed := false
		for _, s := range out {
			loc := reVarRef.FindStringSubmatchIndex(s)
			if loc == nil {
				next = append(next, s)
				continue
			}
			changed = true
			m := reVarRef.FindStringSubmatch(s)
			key := m[1] + m[3]
			vals := ci.values(key, comps)
			if len(vals) == 0 {
				vals = []string{m[2]}
			}
			for _, val := range vals {
				next = append(next, s[:loc[0]]+val+s[loc[1]:])
			}
		}
		out = uniqSorted(next)
		if len(out) > 32 {
			out = out[:32]
		}
		if !changed {
			break
		}
	}
	return out
}

// identities is step 2: which names run, and which components' code each runs.
func (t *topology) identities(nodes []*graph.Node, g *graph.Graph, cfg *configIndex) {
	// Images: the components each Dockerfile copies in beside its own.
	imageRuns := map[string]map[string]bool{} // Dockerfile path -> components
	copiedBy := map[string]map[string]bool{}  // component -> components whose image copies it
	for _, n := range nodes {
		if n.Kind != "Image" {
			continue
		}
		t.images++
		own := t.componentOf(n.FilePath)
		runs := map[string]bool{}
		if own != "" {
			runs[own] = true
		}
		for _, e := range g.Callees(n.QN, parser.EdgeReferences) {
			src, ok := strings.CutPrefix(stripRepo(e.Target), "path:")
			if !ok {
				continue
			}
			if c := t.componentAt(src); c != "" {
				runs[c] = true
				if own != "" && c != own {
					if copiedBy[c] == nil {
						copiedBy[c] = map[string]bool{}
					}
					copiedBy[c][own] = true
				}
			}
		}
		imageRuns[n.FilePath] = runs
	}
	hasImage := map[string]bool{}
	for file := range imageRuns {
		if c := t.componentOf(file); c != "" {
			hasImage[c] = true
		}
	}
	for c := range copiedBy {
		if !hasImage[c] {
			t.libraries[c] = true
		}
	}
	runsOf := func(c string) map[string]bool {
		runs := map[string]bool{c: true}
		for file, r := range imageRuns {
			if t.componentOf(file) == c {
				for x := range r {
					runs[x] = true
				}
			}
		}
		return runs
	}

	// Deployments (compose services) are the identities when there are any.
	type deployment struct {
		name, component string
		runs            map[string]bool
	}
	// One service can be declared by several compose files (compose.yaml and
	// its overlays); their declarations add up.
	var deploys []*deployment
	byName := map[string]*deployment{}
	for _, n := range nodes {
		if n.Kind != "Deployment" {
			continue
		}
		d := byName[n.Name]
		if d == nil {
			d = &deployment{name: n.Name, runs: map[string]bool{}}
			byName[n.Name] = d
			deploys = append(deploys, d)
		}
		for _, e := range g.Callees(n.QN, parser.EdgeReferences) {
			tgt := stripRepo(e.Target)
			if e.Extra["composeRef"] != "build" {
				continue
			}
			for _, df := range cfg.expand(tgt, nil) {
				df = path.Clean(strings.TrimPrefix(df, "./"))
				if c := t.componentAt(path.Dir(df)); c != "" {
					d.component = c
					d.runs = runsOf(c)
					if r, ok := imageRuns[df]; ok {
						d.runs = r
					}
				}
			}
		}
	}
	if len(deploys) > 0 {
		t.deployments = len(deploys)
		for _, d := range deploys {
			s := newService(d.name, "compose service "+d.name)
			s.component, s.runs = d.component, d.runs
			if d.component != "" {
				s.identity = t.components[d.component].root + " (compose service " + d.name + ")"
			}
			t.services[d.name] = s
		}
		return
	}
	for _, name := range sortedKeys(t.components) {
		if t.libraries[name] || (len(hasImage) > 0 && !hasImage[name]) {
			continue
		}
		s := newService(name, "component "+t.components[name].root)
		if hasImage[name] {
			s.identity = "image " + t.components[name].root
		}
		s.component, s.runs = name, runsOf(name)
		t.services[name] = s
	}
}

// endpoints is step 3: the base paths and registered names each service
// answers to, from the config of its own component.
func (t *topology) endpoints(cfg *configIndex) {
	for _, s := range t.services {
		if s.component == "" {
			continue
		}
		// Own config only: a library's or the repo's base path is not this
		// service's.
		own := cfg.byComp[s.component]
		for key := range serverPathKeys {
			for _, p := range own[key] {
				if p = strings.TrimRight(p, "/"); p != "" && !strings.Contains(p, "${") {
					s.paths = append(s.paths, p)
				}
			}
		}
		for key := range serviceNameKeys {
			for _, v := range own[key] {
				if v != "" && !strings.Contains(v, "${") {
					s.registered[v] = true
					// An alias must never equal another identity's own name.
					if v != s.name && t.services[v] == nil {
						s.aliases[v] = true
					}
				}
			}
		}
		s.paths = uniqSorted(s.paths)
	}
}

// grpcCalls: a service is a client of an interface its code uses, and calls
// the one service whose code serves it.
func (t *topology) grpcCalls() {
	servers := map[string][]string{}
	for _, name := range sortedKeys(t.services) {
		s := t.services[name]
		for c := range s.runs {
			comp := t.components[c]
			for iface, ev := range comp.serverOf {
				s.serverOf[iface] = append(s.serverOf[iface], ev...)
			}
			for iface, ev := range comp.clientOf {
				s.clientOf[iface] = append(s.clientOf[iface], ev...)
			}
		}
		for iface := range s.serverOf {
			servers[iface] = append(servers[iface], name)
		}
	}
	for _, name := range sortedKeys(t.services) {
		s := t.services[name]
		for _, iface := range sortedKeys(s.clientOf) {
			srv := uniqSorted(servers[iface])
			switch {
			case len(srv) == 0:
				t.unserved[iface] = true
			case len(srv) > 1:
				t.ambiguous[iface] = srv
			case srv[0] != name:
				s.calls[srv[0]] = append(s.calls[srv[0]], "gRPC "+iface)
			}
		}
	}
}

type declaredClient struct {
	qn, file, class, name, url string
	// imperative marks a client discovered from an imperative call
	// (RestTemplate, WebServiceTemplate) rather than a declared annotation: it
	// carries no framework-given name, so an address matching no in-repo
	// service is named after its host (externalHost) instead of grouped under
	// an empty one.
	imperative bool
}

// clientCalls resolves declared HTTP clients (and records queue listeners).
func (t *topology) clientCalls(nodes []*graph.Node, g *graph.Graph, cfg *configIndex) {
	// Every client declaration, and each file's resolved references to other
	// files: a client is code a service runs only when the service's own code
	// reaches it through them.
	var clients []declaredClient
	clientFile := map[string]bool{}
	type listener struct{ file, dest, qn string }
	var listeners []listener
	fileRefs := map[string]map[string]bool{}
	for _, n := range nodes {
		for _, e := range g.Callees(n.QN, parser.EdgeReferences) {
			if anno := e.Extra[parser.ExtraAnnotation]; anno != "" {
				if spec, ok := httpClientAnnotations[anno]; ok {
					cl := declaredClient{qn: n.QN, file: n.FilePath, class: n.Name, url: e.Extra[parser.ExtraArgPrefix+spec.url]}
					for _, k := range spec.name {
						if v := e.Extra[parser.ExtraArgPrefix+k]; v != "" && cl.name == "" {
							cl.name = v
						}
					}
					if cl.name == "" {
						cl.name = n.Name
					}
					clients = append(clients, cl)
					clientFile[n.FilePath] = true
				}
				if arg, ok := queueListenerAnnotations[anno]; ok {
					if d := e.Extra[parser.ExtraArgPrefix+arg]; d != "" {
						listeners = append(listeners, listener{file: n.FilePath, dest: d, qn: n.QN})
					}
				}
				continue
			}
			if tn, ok := g.Get(e.Target); ok && tn.FilePath != n.FilePath {
				if fileRefs[n.FilePath] == nil {
					fileRefs[n.FilePath] = map[string]bool{}
				}
				fileRefs[n.FilePath][tn.FilePath] = true
			}
		}
	}
	clients = append(clients, imperativeClients(nodes, g)...)
	t.clients = len(clients)
	if len(clients) == 0 && len(listeners) == 0 {
		return
	}
	filesOf := map[string][]string{} // component -> files
	for _, n := range nodes {
		if n.Kind == "File" {
			if c := t.componentOf(n.FilePath); c != "" {
				filesOf[c] = append(filesOf[c], n.FilePath)
			}
		}
	}
	// reached is the code a service runs and reaches: its own component's
	// files (a client declaration only once something references it), then
	// every file of a component it runs that reached code references.
	reached := func(s *topoService) map[string]bool {
		seen := map[string]bool{}
		var queue []string
		for _, f := range filesOf[s.component] {
			if !clientFile[f] {
				seen[f] = true
				queue = append(queue, f)
			}
		}
		for len(queue) > 0 {
			f := queue[0]
			queue = queue[1:]
			for to := range fileRefs[f] {
				if !seen[to] && s.runs[t.componentOf(to)] {
					seen[to] = true
					queue = append(queue, to)
				}
			}
		}
		return seen
	}

	// External endpoints: clients whose address names no service, grouped so
	// one address under two client names is one service, and one name over two
	// addresses of it is too.
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] == "" || parent[x] == x {
			parent[x] = x
			return x
		}
		parent[x] = find(parent[x])
		return parent[x]
	}
	union := func(a, b string) { parent[find(a)] = find(b) }
	type extCall struct {
		from string
		cl   declaredClient
	}
	var extCalls []extCall

	for _, name := range sortedKeys(t.services) {
		s := t.services[name]
		if s.component == "" {
			continue
		}
		code := reached(s)
		for _, cl := range clients {
			if !code[cl.file] {
				if s.runs[t.componentOf(cl.file)] {
					t.unusedClients++
				}
				continue
			}
			ev := fmt.Sprintf("HTTP client %s (%s)", cl.class, strings.TrimSpace(cl.name+" "+cl.url+t.expandedAddr(cl.url, s, cfg)))
			targets := t.resolveAddress(cl, s, cfg)
			if len(targets) == 0 {
				// An imperative call carries no framework-given name to group
				// or display external services by (a Feign client always
				// does); use the address's own host instead, when it expands
				// to one.
				if cl.imperative {
					if host := t.externalHost(cl.url, s, cfg); host != "" {
						cl.name = host
					}
				}
				extCalls = append(extCalls, extCall{from: name, cl: cl})
				key := "name:" + cl.name
				if cl.url != "" {
					union("url:"+cl.url, key)
				}
				find(key)
				continue
			}
			for _, tgt := range targets {
				if tgt == name {
					continue
				}
				s.calls[tgt] = append(s.calls[tgt], ev)
				// An alias must never equal another identity's own name: art-
				// resource-api serving context-path /resource-api-service must
				// not alias the real, separate resource-api-service module.
				if cl.name != "" && cl.name != tgt && t.services[cl.name] == nil {
					t.services[tgt].aliases[cl.name] = true
				}
			}
		}
		for _, l := range listeners {
			if code[l.file] {
				s.consumes[strings.Trim(l.dest, "${}")] = append(s.consumes[strings.Trim(l.dest, "${}")], stripRepo(l.qn))
			}
		}
	}

	groups := map[string][]string{} // root -> client names
	for _, c := range extCalls {
		root := find("name:" + c.cl.name)
		groups[root] = append(groups[root], c.cl.name)
	}
	extName := map[string]string{}
	for root, names := range groups {
		names = uniqSorted(names)
		sort.SliceStable(names, func(i, j int) bool { return len(names[i]) < len(names[j]) })
		extName[root] = names[0]
		if t.services[names[0]] == nil {
			ext := newService(names[0], "outside the repo")
			ext.external = true
			t.services[names[0]] = ext
			t.externals++
		}
		for _, n := range names[1:] {
			if t.services[n] == nil {
				t.services[names[0]].aliases[n] = true
			}
		}
	}
	for _, c := range extCalls {
		tgt := extName[find("name:"+c.cl.name)]
		ev := fmt.Sprintf("HTTP client %s (%s)", c.cl.class, strings.TrimSpace(c.cl.name+" "+c.cl.url+t.expandedAddr(c.cl.url, t.services[c.from], cfg)))
		t.services[c.from].calls[tgt] = append(t.services[c.from].calls[tgt], ev)
	}
}

// resolveAddress is the services a client's address names: the url argument
// expanded through the config the calling service runs, matched on a deployment
// host or the longest served base path; with no url, the name matched on a
// registered or deployment name.
func (t *topology) resolveAddress(cl declaredClient, from *topoService, cfg *configIndex) []string {
	hit := map[string]bool{}
	if cl.url != "" {
		for _, addr := range cfg.expand(cl.url, from.runs) {
			if addr == "" {
				continue
			}
			u, err := url.Parse(addr)
			if err != nil {
				continue
			}
			if t.services[u.Hostname()] != nil && !t.services[u.Hostname()].external {
				hit[u.Hostname()] = true
				continue
			}
			p := strings.TrimRight(u.Path, "/")
			bestLen, owners := 0, []string{}
			for _, name := range sortedKeys(t.services) {
				for _, sp := range t.services[name].paths {
					if p != sp && !strings.HasPrefix(p, sp+"/") {
						continue
					}
					switch {
					case len(sp) > bestLen:
						bestLen, owners = len(sp), []string{name}
					case len(sp) == bestLen:
						owners = append(owners, name)
					}
				}
			}
			if len(owners) == 1 {
				hit[owners[0]] = true
			}
		}
		return sortedKeys(hit)
	}
	for _, name := range sortedKeys(t.services) {
		s := t.services[name]
		if !s.external && (name == cl.name || s.registered[cl.name]) {
			hit[name] = true
		}
	}
	return sortedKeys(hit)
}

// externalHost is the redacted host an imperative call's address expands to
// (scheme, port, path, credentials and query all dropped by url.Hostname()),
// used only once resolveAddress has already found no in-repo match: a Feign
// client already carries a name to group and display external services by,
// and an imperative call does not, so this is its stand-in.
// expandedAddr is " -> <address>" for a url that is a config reference
// expanding to a different address, so evidence keeps the path that names the
// service behind a shared host; query and credentials are dropped.
func (t *topology) expandedAddr(rawURL string, from *topoService, cfg *configIndex) string {
	if from == nil {
		return ""
	}
	for _, addr := range cfg.expand(rawURL, from.runs) {
		if addr == "" || addr == rawURL {
			continue
		}
		u, err := url.Parse(addr)
		if err != nil || u.Hostname() == "" {
			continue
		}
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		return " -> " + u.String()
	}
	return ""
}

func (t *topology) externalHost(rawURL string, from *topoService, cfg *configIndex) string {
	for _, addr := range cfg.expand(rawURL, from.runs) {
		if addr == "" {
			continue
		}
		if u, err := url.Parse(addr); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	return ""
}

// isTestPath is Gradle and Maven's own test convention: src/test/java (or
// .../src/test/... in a multi-module layout). Exercising a client proves
// nothing about what the service calls in production — a TestRestTemplate in
// a JUnit helper is not art-resource-api issuing a call — so test code is
// never a source, an intermediate frame, or a resolved caller of one.
func isTestPath(file string) bool {
	return strings.Contains(file, "/src/test/") || strings.HasPrefix(file, "src/test/")
}

// imperativeHit is an imperative HTTP/SOAP call's address, resolved back to
// whichever call frame supplied it as a literal or a config value, and that
// frame's own file (what reached() checks to attribute the call to a
// service).
type imperativeHit struct {
	url, fromQN, file string
}

// imperativeClients finds RestTemplate, WebClient and Spring-WS calls whose
// address traces to a literal or a Spring config value, producing
// declaredClient entries the same reach-and-resolve loop in clientCalls reads
// a declared client from. The address is usually not written at the call
// that issues the request — a hand-written HTTP client is ordinarily an
// interface plus one impl, and the impl's method just forwards what it was
// given (java_calls.go marks this ExtraArgURLParam0) — so resolution walks
// back through the method's own callers, through its interface when it has
// one, until a caller supplies a literal or a field's Spring @Value(...)
// (java_calls.go resolves that at parse time, same as this function reads a
// literal argument).
func imperativeClients(nodes []*graph.Node, g *graph.Graph) []declaredClient {
	var out []declaredClient
	for _, n := range nodes {
		if n.Kind != "Func" || isTestPath(n.FilePath) {
			continue
		}
		for _, e := range g.Callees(n.QN, parser.EdgeCalls) {
			name := lastSegment(e.Target)
			if !imperativeHTTPCallNames[name] && !(genericHTTPCallNames[name] && httpClientTypes[e.Extra["recvType"]]) {
				continue
			}
			for _, hit := range resolveImperativeAddr(g, e, n.QN, map[string]bool{}, 0) {
				if hit.url == "" || isTestPath(hit.file) {
					continue
				}
				out = append(out, declaredClient{
					qn: hit.fromQN, file: hit.file, class: lastSegment(hit.fromQN),
					name: hit.url, url: hit.url, imperative: true,
				})
			}
		}
	}
	return out
}

// resolveImperativeAddr is a call's own address argument (ExtraArgPrefix+
// "url", resolved at parse time), or — when it forwards the enclosing
// method's own first parameter (ExtraArgURLParam0) — the same resolution run
// on each of that method's own callers instead: direct callers, and callers
// of its interface method when it implements one, since code elsewhere
// ordinarily holds the interface type, not the impl. Five frames deep at
// most, cfg.expand's own bound: a chain longer than a wrapper and its one
// caller gives up rather than guess.
func resolveImperativeAddr(g *graph.Graph, e graph.Edge, enclosing string, seen map[string]bool, depth int) []imperativeHit {
	if v := e.Extra[parser.ExtraArgPrefix+"url"]; v != "" {
		file := ""
		if nd, ok := g.Get(enclosing); ok {
			file = nd.FilePath
		}
		return []imperativeHit{{url: v, fromQN: enclosing, file: file}}
	}
	if e.Extra[parser.ExtraArgURLParam0] == "" || seen[enclosing] || depth >= 5 {
		return nil
	}
	seen[enclosing] = true
	targets := []string{enclosing}
	if iface := interfaceMethodOf(g, enclosing); iface != "" {
		targets = append(targets, iface)
	}
	var hits []imperativeHit
	for _, tq := range targets {
		for _, caller := range g.Callers(tq, parser.EdgeCalls) {
			if nd, ok := g.Get(caller.Source); ok && isTestPath(nd.FilePath) {
				continue
			}
			hits = append(hits, resolveImperativeAddr(g, caller, caller.Source, seen, depth+1)...)
		}
	}
	return hits
}

// interfaceMethodOf is the interface method an impl method implements: try
// every type the enclosing class references — its interfaces, its
// superclass, anything a references edge points at — for one where the
// method's own name, appended, names a Func the graph actually holds.
// Callers of a hand-written HTTP client wrapper go through its interface
// field, not the impl, so resolveImperativeAddr must look there too.
func interfaceMethodOf(g *graph.Graph, implMethodQN string) string {
	i := strings.LastIndex(implMethodQN, ".")
	if i < 0 {
		return ""
	}
	classQN, method := implMethodQN[:i], implMethodQN[i+1:]
	for _, e := range g.Callees(classQN, parser.EdgeReferences) {
		cand := e.Target + "." + method
		if cand == implMethodQN {
			continue
		}
		if nd, ok := g.Get(cand); ok && nd.Kind == "Func" {
			return cand
		}
	}
	return ""
}

// telemetryExportKey matches the OpenTelemetry SDK's exporter configuration
// (OTEL_EXPORTER_OTLP_ENDPOINT and the rest of the OTEL_EXPORTER_ namespace the
// spec defines). An address given there is where a process ships its traces,
// metrics and logs, not a service it depends on to do its work; counted as a
// call, it made the collector a dependency of every service in the OpenTelemetry
// demo (29 of 52 edges its architecture does not have) and the largest blast
// radius in Triage.
//
// The deployments those keys resolve to are the telemetry sinks, and no env
// address to a sink is a call, whatever the key is named: nginx and Envoy reach
// the same collector through OTEL_COLLECTOR_HOST, the Traceloop SDK through
// TRACELOOP_BASE_URL. The sink is found from the spec's keys, not by name.
var telemetryExportKey = regexp.MustCompile(`^OTEL_EXPORTER_`)

// envCalls: a deployment calls another when an environment key it passes
// expands, through Config nodes, to a value naming that deployment as a host:
// CART_ADDR=cart:${CART_PORT}, KAFKA_ADDR=${KAFKA_HOST}:…, a connection
// string's @${POSTGRES_HOST}/. The key is what the process reads; the value is
// where it goes.
func (t *topology) envCalls(nodes []*graph.Node, g *graph.Graph, cfg *configIndex) {
	if t.deployments == 0 {
		return
	}
	names := sortedKeys(t.services)
	hostRe := map[string]*regexp.Regexp{}
	for _, d := range names {
		hostRe[d] = regexp.MustCompile(`(?:^|[/@=;,\s(])` + regexp.QuoteMeta(d) + `(?:$|[:/;,\s)?])`)
	}
	type envRef struct{ from, key, value string }
	var refs []envRef
	for _, n := range nodes {
		if n.Kind != "Deployment" || t.services[n.Name] == nil {
			continue
		}
		for _, e := range g.Callees(n.QN, parser.EdgeReferences) {
			key, ok := strings.CutPrefix(stripRepo(e.Target), "env:")
			if !ok {
				continue
			}
			raw, inline := e.Extra["value"]
			if !inline {
				raw = cfg.dotenv[key]
			}
			for _, v := range cfg.expand(raw, nil) {
				refs = append(refs, envRef{from: n.Name, key: key, value: v})
			}
		}
	}
	sinks := map[string]bool{}
	for _, r := range refs {
		if !telemetryExportKey.MatchString(r.key) {
			continue
		}
		for _, callee := range names {
			if hostRe[callee].MatchString(r.value) {
				sinks[callee] = true
			}
		}
	}
	for _, r := range refs {
		for _, callee := range names {
			if callee != r.from && !sinks[callee] && hostRe[callee].MatchString(r.value) {
				s := t.services[r.from]
				s.calls[callee] = append(s.calls[callee], "env "+r.key)
			}
		}
	}
	t.telemetrySinks = sortedKeys(sinks)
}

// dropVariants removes a deployment built from a component another deployment
// is addressed as: frontendTests, the Cypress image of src/frontend. It runs
// before gRPC roles are read, or the component's server would have two owners.
func (t *topology) dropVariants() {
	called := map[string]bool{}
	for _, s := range t.services {
		for c := range s.calls {
			called[c] = true
		}
	}
	byComp := map[string][]string{}
	for _, name := range sortedKeys(t.services) {
		if c := t.services[name].component; c != "" {
			byComp[c] = append(byComp[c], name)
		}
	}
	// One component, several deployments: the service is the one others
	// address; the rest are variants of its code. Handing the component to the
	// last name sorted gave frontend's gRPC calls to frontendTests.
	for _, names := range byComp {
		if len(names) < 2 {
			continue
		}
		var addressed []string
		for _, n := range names {
			if called[n] {
				addressed = append(addressed, n)
			}
		}
		if len(addressed) != 1 {
			continue
		}
		for _, n := range names {
			if n != addressed[0] {
				delete(t.services, n)
			}
		}
	}
}

// triage renders the topology as a Triage services.yaml. Every identity is an
// entry, called or not: a service nothing calls is still a node the classifier
// should know exists. called_by mirrors calls, so the file lints.
func (t *topology) triage(tier, extTier int, team string) triageCatalog {
	calledBy := map[string][]string{}
	for _, name := range sortedKeys(t.services) {
		for callee := range t.services[name].calls {
			calledBy[callee] = append(calledBy[callee], name)
		}
	}
	// A domain is a name prefix two in-repo services share;
	// otherwise the container, or the repo when the container is its root.
	prefixCount := map[string]int{}
	for _, s := range t.services {
		if !s.external {
			prefixCount[domainOf(s.name)]++
		}
	}
	var doc triageCatalog
	for _, name := range sortedKeys(t.services) {
		s := t.services[name]
		domain := domainOf(name)
		if prefixCount[domain] < 2 {
			domain = t.container
		}
		svcTier := tier
		if s.external {
			domain, svcTier = "external", extTier
		}
		svc := triageService{
			Name:     name,
			Aliases:  sortedKeys(s.aliases),
			Domain:   domain,
			Tier:     svcTier,
			Team:     teamOr(team, domain),
			Calls:    sortedKeys(s.calls),
			CalledBy: uniqSorted(calledBy[name]),
		}
		var b strings.Builder
		if s.external {
			fmt.Fprintf(&b, "Service outside the repo, called by %s.", strings.Join(svc.CalledBy, ", "))
		} else {
			fmt.Fprintf(&b, "Service %s.", s.identity)
			if libs := t.librariesOf(s); len(libs) > 0 {
				fmt.Fprintf(&b, " Runs library %s.", strings.Join(libs, ", "))
			}
			if len(s.paths) > 0 {
				fmt.Fprintf(&b, " Serves %s.", strings.Join(s.paths, ", "))
			}
			if len(s.serverOf) > 0 {
				fmt.Fprintf(&b, " Serves gRPC %s.", strings.Join(sortedKeys(s.serverOf), ", "))
			}
		}
		var parts []string
		for _, callee := range svc.Calls {
			parts = append(parts, fmt.Sprintf("%s (%s)", callee, strings.Join(uniqSorted(s.calls[callee]), ", ")))
		}
		if len(parts) > 0 {
			fmt.Fprintf(&b, " Calls %s.", strings.Join(parts, "; "))
		}
		if len(s.consumes) > 0 {
			fmt.Fprintf(&b, " Consumes %s.", strings.Join(sortedKeys(s.consumes), ", "))
		}
		svc.Description = b.String()
		svc.Symptoms = t.symptoms(s, svc.Calls)
		doc.Services = append(doc.Services, svc)
	}
	return doc
}

// symptoms is a service's failure vocabulary, from how its calls are made.
func (t *topology) symptoms(s *topoService, calls []string) string {
	var grpc, http, env []string
	for _, c := range calls {
		ev := strings.Join(s.calls[c], " ")
		switch {
		case strings.Contains(ev, "gRPC "):
			grpc = append(grpc, c)
		case strings.Contains(ev, "HTTP client "):
			http = append(http, c)
		default:
			env = append(env, c)
		}
	}
	var parts []string
	if len(grpc) > 0 {
		parts = append(parts, "RPC errors, deadline exceeded or UNAVAILABLE calling "+strings.Join(grpc, ", "))
	}
	if len(http) > 0 {
		parts = append(parts, "HTTP client errors (FeignException, RetryableException, connection refused, read timed out, 5xx) calling "+strings.Join(http, ", "))
	}
	if len(env) > 0 {
		parts = append(parts, "connection refused or timeouts reaching "+strings.Join(env, ", "))
	}
	for _, q := range sortedKeys(s.consumes) {
		parts = append(parts, "listener errors or backlog on "+q)
	}
	if s.external {
		return fmt.Sprintf("connection refused, read timed out or 5xx from %s", s.name)
	}
	return strings.Join(append(parts, "errors, 5xx responses or restarts in "+s.name), "; ")
}

func (t *topology) librariesOf(s *topoService) []string {
	var libs []string
	for c := range s.runs {
		if c != s.component {
			libs = append(libs, c)
		}
	}
	sort.Strings(libs)
	return libs
}

// catalog renders the topology as entity-catalog entries for servicemap.
func (t *topology) catalog() []catalogEntry {
	doc := t.triage(0, 0, "")
	var entries []catalogEntry
	for _, svc := range doc.Services {
		s := t.services[svc.Name]
		kind := "Service"
		if s.external {
			kind = "ExternalService"
		}
		e := catalogEntry{Name: svc.Name, Kind: kind, Text: svc.Description, Attrs: map[string]string{}}
		set := func(k string, vs []string) {
			if len(vs) > 0 {
				e.Attrs[k] = strings.Join(vs, ", ")
			}
		}
		set("aliases", svc.Aliases)
		set("calls", svc.Calls)
		set("called_by", svc.CalledBy)
		set("libraries", t.librariesOf(s))
		set("paths", s.paths)
		set("consumes", sortedKeys(s.consumes))
		for _, c := range svc.Calls {
			e.Links = append(e.Links, catalogLink{Rel: parser.EdgeCalls, To: c})
			for _, ev := range uniqSorted(s.calls[c]) {
				e.Parts = append(e.Parts, catalogPart{Name: svc.Name + " -> " + c + ": " + ev, Kind: "Call", Text: ev})
			}
		}
		for _, l := range t.librariesOf(s) {
			e.Links = append(e.Links, catalogLink{Rel: parser.EdgeImports, To: l})
		}
		entries = append(entries, e)
	}
	return entries
}

func (t *topology) stats() string {
	edges, servers, clients := 0, 0, 0
	for _, s := range t.services {
		edges += len(s.calls)
		servers += len(s.serverOf)
		clients += len(s.clientOf)
	}
	return fmt.Sprintf("container=%s components=%d images=%d libraries=%v deployments=%d services=%d external=%d calls=%d httpClients=%d unreachedClientUses=%d interfaces=%d generatedFiles=%d grpcServerRoles=%d grpcClientRoles=%d telemetrySinks=%v unserved=%v ambiguous=%v",
		t.container, len(t.components), t.images, sortedKeys(t.libraries), t.deployments, len(t.services)-t.externals, t.externals, edges,
		t.clients, t.unusedClients, len(t.interfaces), len(t.generated), servers, clients, t.telemetrySinks, sortedKeys(t.unserved), t.ambiguous)
}
