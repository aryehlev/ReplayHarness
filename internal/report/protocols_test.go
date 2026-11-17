package report

import (
	"strings"
	"testing"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

func TestOtherProtocolCommands(t *testing.T) {
	o := Options{Target: "http://127.0.0.1:8081", GRPCTarget: "127.0.0.1:9091",
		Headers: map[string]string{"Authorization": "Bearer s3cret"}, AuthHeaders: map[string]bool{"Authorization": true}}
	grpcSc := model.Scenario{Request: model.Request{Protocol: model.ProtoGRPC, Method: "GRPC", Path: "/users.v1.Users/GetUser", Body: `{"id":"it's"}`}}
	got := Command(o, grpcSc)
	want := `grpcurl -plaintext -H 'authorization: <redacted>' -d '{"id":"it'\''s"}' 127.0.0.1:9091 users.v1.Users/GetUser`
	if got != want {
		t.Errorf("grpcurl:\n got %s\nwant %s", got, want)
	}
	grpcSc.StripAuth = true
	if strings.Contains(Command(o, grpcSc), "authorization") {
		t.Error("a no-credentials scenario shows the credentials header")
	}
	o.GRPCTarget = "grpcs://api.internal:443"
	if got := Command(o, grpcSc); strings.Contains(got, "-plaintext") || !strings.Contains(got, " api.internal:443 ") {
		t.Errorf("TLS grpcurl: %s", got)
	}

	ws := model.Scenario{Request: model.Request{Protocol: model.ProtoWebSocket, Method: "GET", Path: "/ws/chat", Query: "room=a",
		Body: `[{"type":"join"},"plain text"]`}}
	got = Command(o, ws)
	want = `printf '%s\n' '{"type":"join"}' 'plain text' | websocat -H 'Authorization: <redacted>' 'ws://127.0.0.1:8081/ws/chat?room=a'`
	if got != want {
		t.Errorf("websocat:\n got %s\nwant %s", got, want)
	}
	o.Target = "https://app.internal"
	if got := Command(o, ws); !strings.Contains(got, "'wss://app.internal/ws/chat?room=a'") {
		t.Errorf("wss: %s", got)
	}
}
