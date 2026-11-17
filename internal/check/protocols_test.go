package check

import (
	"strings"
	"testing"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

func TestOtherProtocolBasics(t *testing.T) {
	findings := func(o model.Observed, proto string) string {
		var out []string
		for _, f := range Basic(o, proto, false, 0) {
			out = append(out, f.Check+": "+f.Message)
		}
		return strings.Join(out, "\n")
	}
	gqlCrash := model.Observed{Status: 200, Body: `{"data":null,"errors":[{"message":"internal system error"}]}`}
	if got := findings(gqlCrash, model.ProtoGraphQL); !strings.Contains(got, "server-error: GraphQL error that looks like a server failure") {
		t.Errorf("GraphQL crash: %q", got)
	}
	if got := findings(model.Observed{Status: 200, Body: `{"errors":[{"message":"not found"}]}`}, model.ProtoGraphQL); got != "" {
		t.Errorf("an ordinary GraphQL error is not a failure: %q", got)
	}
	if o, _ := Effective(model.Observed{Status: 200, Body: `{"errors":[{"message":"bad id"}]}`}, model.ProtoGraphQL); o.Status != 400 || o.Code != "200 with GraphQL errors" {
		t.Errorf("GraphQL rejection: %+v", o)
	}
	if got := findings(model.Observed{Status: 101, Close: 1006, Error: "EOF"}, model.ProtoWebSocket); !strings.Contains(got, "connection: the server dropped the WebSocket") {
		t.Errorf("WebSocket drop: %q", got)
	}
	if got := findings(model.Observed{Status: 101, Close: 1011}, model.ProtoWebSocket); !strings.Contains(got, "closed the WebSocket with 1011") {
		t.Errorf("WebSocket 1011: %q", got)
	}
	if got := findings(model.Observed{Status: 101, Close: 1008}, model.ProtoWebSocket); got != "" {
		t.Errorf("a policy close is a rejection, not a failure: %q", got)
	}
	if got := findings(model.Observed{Status: 500, Code: "INTERNAL", Body: `{"code":"INTERNAL","message":"boom"}`}, model.ProtoGRPC); !strings.Contains(got, "server-error") {
		t.Errorf("gRPC INTERNAL: %q", got)
	}
	if got := findings(model.Observed{Unsent: "id: not an int64"}, model.ProtoGRPC); got != "" {
		t.Errorf("an unsent request has no findings: %q", got)
	}
}
