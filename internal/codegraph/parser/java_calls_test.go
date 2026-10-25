//go:build cgo

package parser

import "testing"

// A hand-written HTTP client (RestTemplate, WebServiceTemplate) usually does
// not write its address at the call that issues the request: an interface's
// impl just forwards whatever it was given. javaCalls resolves a call's first
// argument the same way it resolves a receiver's declared type — from
// evidence in scope, never a guess — so the service map can walk the address
// back through the call graph instead of re-parsing.
func TestJavaCalls_ResolvesAddressArguments(t *testing.T) {
	src := `package com.acme.gizmo;

public class GizmoClient {
    @Value("${ext.url}")
    private String extUrl;

    private static final String FIXED = "https://fixed.example.com/v1";

    public void directField(Sender sender, String body) {
        sender.send(extUrl, body);
    }

    public void literalArg(Sender sender, String body) {
        sender.send(FIXED, body);
    }

    public void stringLiteralInline(Sender sender, String body) {
        sender.send("https://inline.example.com/v1", body);
    }

    public void forwardsOwnFirstParam(String url, String body) {
        restTemplate.postForEntity(url, body, Resp.class);
    }

    public void forwardsSecondParamNotFirst(String body, String url) {
        restTemplate.postForEntity(url, body, Resp.class);
    }

    public void unresolvableChain(Loader loader, String body) {
        String url = loader.getConfig().getUrl() + SUFFIX;
        restTemplate.postForEntity(url, body, Resp.class);
    }
}
`
	res, err := New().ParseFile("svc/src/main/java/com/acme/gizmo/GizmoClient.java", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	byTargetAndLine := map[string]EdgeInfo{}
	for _, e := range res.Edges {
		if e.Kind != EdgeCalls {
			continue
		}
		byTargetAndLine[e.Extra["callName"]+"@"+e.Source] = e
	}

	get := func(callName, enclosing string) EdgeInfo {
		e, ok := byTargetAndLine[callName+"@svc/src/main/java/com/acme/gizmo/GizmoClient.java::GizmoClient."+enclosing]
		if !ok {
			t.Fatalf("no %s call recorded inside %s (have: %v)", callName, enclosing, byTargetAndLine)
		}
		return e
	}

	if e := get("send", "directField"); e.Extra[ExtraArgPrefix+"url"] != "${ext.url}" {
		t.Errorf("directField: arg.url = %q, want the field's @Value placeholder", e.Extra[ExtraArgPrefix+"url"])
	}
	if e := get("send", "literalArg"); e.Extra[ExtraArgPrefix+"url"] != "https://fixed.example.com/v1" {
		t.Errorf("literalArg: arg.url = %q, want the constant's literal", e.Extra[ExtraArgPrefix+"url"])
	}
	if e := get("send", "stringLiteralInline"); e.Extra[ExtraArgPrefix+"url"] != "https://inline.example.com/v1" {
		t.Errorf("stringLiteralInline: arg.url = %q, want the inline literal", e.Extra[ExtraArgPrefix+"url"])
	}
	if e := get("postForEntity", "forwardsOwnFirstParam"); e.Extra[ExtraArgURLParam0] != "1" || e.Extra[ExtraArgPrefix+"url"] != "" {
		t.Errorf("forwardsOwnFirstParam: argUrlParam0=%q arg.url=%q, want argUrlParam0=1 and no resolved value",
			e.Extra[ExtraArgURLParam0], e.Extra[ExtraArgPrefix+"url"])
	}
	if e := get("postForEntity", "forwardsSecondParamNotFirst"); e.Extra[ExtraArgURLParam0] != "" || e.Extra[ExtraArgPrefix+"url"] != "" {
		t.Errorf("forwardsSecondParamNotFirst: argUrlParam0=%q arg.url=%q, want neither — url is parameter 1, not 0",
			e.Extra[ExtraArgURLParam0], e.Extra[ExtraArgPrefix+"url"])
	}
	if e := get("postForEntity", "unresolvableChain"); e.Extra[ExtraArgURLParam0] != "" || e.Extra[ExtraArgPrefix+"url"] != "" {
		t.Errorf("unresolvableChain: argUrlParam0=%q arg.url=%q, want neither — a chained getter plus concatenation is not config-shaped",
			e.Extra[ExtraArgURLParam0], e.Extra[ExtraArgPrefix+"url"])
	}
}

// A local_variable_declaration inside a static or instance initializer block
// runs under the enclosing class scope, not a method scope: walk() pushes a
// scope for a method, constructor or lambda, never for an initializer block.
// That scope's localValues (and vars) is nil, so declareLocalValue used to
// panic writing into it (assignment to entry in nil map) the first time such
// code was parsed — this pins that it no longer does.
func TestJavaCalls_LocalDeclarationOutsideAMethodBodyDoesNotPanic(t *testing.T) {
	src := `package com.acme.gizmo;

public class GizmoConfig {
    static String STATIC_URL;
    String instanceUrl;

    static {
        String tmp = "https://static.example.com/v1";
        STATIC_URL = tmp;
    }

    {
        String tmp = "https://instance.example.com/v1";
        instanceUrl = tmp;
    }
}
`
	if _, err := New().ParseFile("svc/src/main/java/com/acme/gizmo/GizmoConfig.java", []byte(src)); err != nil {
		t.Fatal(err)
	}
}
