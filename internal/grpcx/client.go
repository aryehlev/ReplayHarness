// Package grpcx sends gRPC requests without generated code. Request and
// response messages are JSON on the ReplayHarness side and protobuf on the
// wire; the message types come from server reflection or from the .proto
// files in the project.
package grpcx

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

// Client calls one gRPC server. It is safe for concurrent use.
type Client struct {
	addr    string
	conn    *grpc.ClientConn
	timeout time.Duration
	protos  []string // .proto files or directories to read message types from
	maxBody int

	mu       sync.Mutex
	loaded   bool
	local    *protoregistry.Files // compiled from the .proto files
	own      map[string]string    // the project's own files among them
	notes    []string
	reflect  *reflector
	services map[string]protoreflect.ServiceDescriptor
}

// ParseTarget accepts host:port, grpc://host:port or grpcs://host:port (TLS)
// and returns the address and whether to use TLS.
func ParseTarget(raw string) (addr string, useTLS bool, err error) {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", false, err
		}
		switch u.Scheme {
		case "grpc", "http", "h2c":
		case "grpcs", "https":
			useTLS = true
		default:
			return "", false, fmt.Errorf("gRPC target %q: use host:port, grpc://host:port or grpcs://host:port", raw)
		}
		raw = u.Host
	}
	if _, _, err := net.SplitHostPort(raw); err != nil {
		return "", false, fmt.Errorf("gRPC target %q: use host:port, grpc://host:port or grpcs://host:port", raw)
	}
	return raw, useTLS, nil
}

// New prepares a client. It does not connect until the first call.
func New(target string, protos []string, timeout time.Duration, maxBody int) (*Client, error) {
	addr, useTLS, err := ParseTarget(target)
	if err != nil {
		return nil, err
	}
	creds := insecure.NewCredentials()
	if useTLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20)))
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if maxBody <= 0 {
		maxBody = 1 << 20
	}
	c := &Client{addr: addr, conn: conn, timeout: timeout, protos: protos, maxBody: maxBody, services: map[string]protoreflect.ServiceDescriptor{}}
	c.reflect = &reflector{conn: conn}
	return c, nil
}

// Addr is the server's host:port.
func (c *Client) Addr() string { return c.addr }

// Close closes the connection.
func (c *Client) Close() { _ = c.conn.Close() }

func (c *Client) loadLocal() {
	if c.loaded {
		return
	}
	c.loaded = true
	if len(c.protos) == 0 {
		return
	}
	set := Compile(c.protos)
	c.local, c.own, c.notes = set.Files, set.Own, set.Notes
}

// Service finds a service by its full name, in the .proto files first and
// then through server reflection.
func (c *Client) Service(ctx context.Context, name string) (protoreflect.ServiceDescriptor, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sd, ok := c.services[name]; ok {
		return sd, nil
	}
	c.loadLocal()
	if c.local != nil {
		if d, err := c.local.FindDescriptorByName(protoreflect.FullName(name)); err == nil {
			if sd, ok := d.(protoreflect.ServiceDescriptor); ok {
				c.services[name] = sd
				return sd, nil
			}
		}
	}
	files, err := c.reflect.fileContaining(ctx, name)
	if err != nil {
		where := "the server does not support reflection"
		if !errors.Is(err, errNoReflection) {
			where = "reflection failed: " + err.Error()
		}
		if len(c.protos) > 0 {
			where += ", and no .proto file in " + strings.Join(c.protos, ", ") + " defines it"
		} else {
			where += "; pass --proto DIR to read the .proto files"
		}
		return nil, fmt.Errorf("unknown gRPC service %s: %s", name, where)
	}
	d, err := files.FindDescriptorByName(protoreflect.FullName(name))
	if err != nil {
		return nil, fmt.Errorf("unknown gRPC service %s: the server does not list it", name)
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("%s is not a service", name)
	}
	c.services[name] = sd
	return sd, nil
}

// SplitMethod turns "/pkg.Svc/Method", "pkg.Svc/Method" or "pkg.Svc.Method"
// into the service and method names.
func SplitMethod(full string) (service, method string, err error) {
	full = strings.TrimPrefix(strings.TrimSpace(full), "/")
	if i := strings.LastIndex(full, "/"); i > 0 {
		service, method = full[:i], full[i+1:]
	} else if i := strings.LastIndex(full, "."); i > 0 {
		service, method = full[:i], full[i+1:]
	}
	if service == "" || method == "" || strings.Contains(service, "/") {
		return "", "", fmt.Errorf("gRPC method %q: use package.Service/Method, e.g. users.v1.Users/GetUser", full)
	}
	return service, method, nil
}

// Method finds a method descriptor.
func (c *Client) Method(ctx context.Context, full string) (protoreflect.MethodDescriptor, error) {
	svc, m, err := SplitMethod(full)
	if err != nil {
		return nil, err
	}
	sd, err := c.Service(ctx, svc)
	if err != nil {
		return nil, err
	}
	md := sd.Methods().ByName(protoreflect.Name(m))
	if md == nil {
		var names []string
		for i := 0; i < sd.Methods().Len(); i++ {
			names = append(names, string(sd.Methods().Get(i).Name()))
		}
		return nil, fmt.Errorf("%s has no method %s; it has %s", svc, m, strings.Join(names, ", "))
	}
	return md, nil
}

// Invoke sends one call. Body is the request message as JSON, or a JSON
// array of messages for client and bidi streaming methods. headers become
// metadata. A call that could not be built has Unsent set.
func (c *Client) Invoke(ctx context.Context, req model.Request, headers map[string]string) model.Observed {
	md, err := c.Method(ctx, req.Path)
	if err != nil {
		return model.Observed{Unsent: err.Error()}
	}
	msgs, err := c.encode(md, req.BodyBytes())
	if err != nil {
		return model.Observed{Unsent: err.Error()}
	}
	pairs := metadata.MD{}
	for k, v := range headers {
		k = strings.ToLower(k)
		if k == "content-type" || k == "user-agent" || k == "te" || k == "host" || k == "connection" || strings.HasPrefix(k, "grpc-") {
			continue
		}
		pairs.Append(k, v)
	}
	ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(ctx, pairs), c.timeout)
	defer cancel()
	full := "/" + string(md.Parent().FullName()) + "/" + string(md.Name())
	start := time.Now()
	var replies []proto.Message
	var hdr, trl metadata.MD
	if !md.IsStreamingClient() && !md.IsStreamingServer() {
		out := dynamicpb.NewMessage(md.Output())
		err = c.conn.Invoke(ctx, full, msgs[0], out, grpc.Header(&hdr), grpc.Trailer(&trl))
		if err == nil {
			replies = append(replies, out)
		}
	} else {
		replies, hdr, trl, err = c.stream(ctx, md, full, msgs)
	}
	elapsed := time.Since(start)
	obs := model.Observed{DurationMS: float64(elapsed.Microseconds()) / 1000, Headers: map[string]string{}}
	for _, m := range []metadata.MD{hdr, trl} {
		for k, v := range m {
			obs.Headers[k] = strings.Join(v, ", ")
		}
	}
	st := status.Convert(err)
	if dropped(st) {
		obs.Error = st.Message()
		return obs
	}
	if st.Code() == codes.DeadlineExceeded && ctx.Err() != nil {
		obs.Error, obs.Timeout = "no response within "+c.timeout.String(), true
		return obs
	}
	obs.Code = CodeName(st.Code())
	obs.Status = HTTPStatus(st.Code())
	var body any
	if err == nil {
		if md.IsStreamingServer() {
			list := make([]any, 0, len(replies))
			for _, r := range replies {
				list = append(list, c.toJSON(r))
			}
			body = list
		} else if len(replies) > 0 {
			body = c.toJSON(replies[0])
		}
	} else {
		e := map[string]any{"code": obs.Code, "message": st.Message()}
		var details []any
		for _, d := range st.Proto().GetDetails() {
			var v any
			if b, err := (protojson.MarshalOptions{Resolver: c.resolver()}).Marshal(d); err == nil && json.Unmarshal(b, &v) == nil {
				details = append(details, v)
			} else {
				details = append(details, map[string]any{"@type": d.GetTypeUrl()})
			}
		}
		if len(details) > 0 {
			e["details"] = details
		}
		if len(replies) > 0 {
			list := make([]any, 0, len(replies))
			for _, r := range replies {
				list = append(list, c.toJSON(r))
			}
			e["messages"] = list
		}
		body = e
	}
	if body != nil {
		b, _ := json.Marshal(body)
		if len(b) > c.maxBody {
			b, obs.Truncated = b[:c.maxBody], true
		}
		obs.Body = string(b)
	}
	return obs
}

func (c *Client) stream(ctx context.Context, md protoreflect.MethodDescriptor, full string, msgs []proto.Message) ([]proto.Message, metadata.MD, metadata.MD, error) {
	desc := &grpc.StreamDesc{StreamName: string(md.Name()), ServerStreams: md.IsStreamingServer(), ClientStreams: md.IsStreamingClient()}
	s, err := c.conn.NewStream(ctx, desc, full)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, m := range msgs {
		if err := s.SendMsg(m); err != nil {
			break // the error comes back from RecvMsg
		}
	}
	_ = s.CloseSend()
	var out []proto.Message
	for len(out) < 1000 {
		m := dynamicpb.NewMessage(md.Output())
		err = s.RecvMsg(m)
		if err != nil {
			break
		}
		out = append(out, m)
		if !md.IsStreamingServer() {
			err = nil
			break
		}
	}
	if errors.Is(err, io.EOF) {
		err = nil
	}
	hdr, _ := s.Header()
	return out, hdr, s.Trailer(), err
}

func (c *Client) encode(md protoreflect.MethodDescriptor, body []byte) ([]proto.Message, error) {
	in := md.Input()
	opts := protojson.UnmarshalOptions{Resolver: c.resolver()}
	one := func(raw []byte) (proto.Message, error) {
		m := dynamicpb.NewMessage(in)
		if len(strings.TrimSpace(string(raw))) == 0 || string(raw) == "null" {
			return m, nil
		}
		if err := opts.Unmarshal(raw, m); err != nil {
			return nil, fmt.Errorf("the message does not fit %s: %v (fields: %s)", in.FullName(), cleanErr(err), FieldList(in))
		}
		return m, nil
	}
	if !md.IsStreamingClient() {
		m, err := one(body)
		if err != nil {
			return nil, err
		}
		return []proto.Message{m}, nil
	}
	var raw []json.RawMessage
	trimmed := strings.TrimSpace(string(body))
	switch {
	case trimmed == "":
	case strings.HasPrefix(trimmed, "["):
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("messages: %v", err)
		}
	default:
		raw = []json.RawMessage{body}
	}
	out := make([]proto.Message, 0, len(raw))
	for i, r := range raw {
		m, err := one(r)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i+1, err)
		}
		out = append(out, m)
	}
	return out, nil
}

func cleanErr(err error) string {
	s := err.Error()
	s = strings.TrimPrefix(s, "proto: ")
	return strings.ReplaceAll(s, "(line 1:", "(at")
}

// FieldList names a message's fields, e.g. "id (int64), name (string)".
func FieldList(m protoreflect.MessageDescriptor) string {
	var parts []string
	for i := 0; i < m.Fields().Len(); i++ {
		f := m.Fields().Get(i)
		t := f.Kind().String()
		if f.Message() != nil {
			t = string(f.Message().Name())
		} else if f.Enum() != nil {
			t = string(f.Enum().Name())
		}
		if f.IsList() {
			t = "repeated " + t
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", f.Name(), t))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func (c *Client) resolver() interface {
	protoregistry.ExtensionTypeResolver
	protoregistry.MessageTypeResolver
} {
	return dynamicResolver{c}
}

func (c *Client) toJSON(m proto.Message) any {
	b, err := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true, Resolver: c.resolver()}.Marshal(m)
	if err != nil {
		return map[string]any{"replayharness_error": "could not render the response: " + err.Error()}
	}
	var v any
	_ = json.Unmarshal(b, &v)
	return v
}

// dropped reports whether the call never got an answer from the server: the
// connection was refused, reset or closed.
func dropped(st *status.Status) bool {
	m := strings.ToLower(st.Message())
	switch st.Code() {
	case codes.Unavailable:
		for _, s := range []string{"connection error", "error reading from server", "connection refused", "connection reset",
			"transport is closing", "eof", "broken pipe", "connection closed"} {
			if strings.Contains(m, s) {
				return true
			}
		}
	case codes.Internal, codes.Unknown:
		return strings.Contains(m, "stream terminated by rst_stream") || strings.Contains(m, "server closed the stream without sending trailers")
	}
	return false
}

var codeNames = map[codes.Code]string{
	codes.OK: "OK", codes.Canceled: "CANCELLED", codes.Unknown: "UNKNOWN", codes.InvalidArgument: "INVALID_ARGUMENT",
	codes.DeadlineExceeded: "DEADLINE_EXCEEDED", codes.NotFound: "NOT_FOUND", codes.AlreadyExists: "ALREADY_EXISTS",
	codes.PermissionDenied: "PERMISSION_DENIED", codes.ResourceExhausted: "RESOURCE_EXHAUSTED",
	codes.FailedPrecondition: "FAILED_PRECONDITION", codes.Aborted: "ABORTED", codes.OutOfRange: "OUT_OF_RANGE",
	codes.Unimplemented: "UNIMPLEMENTED", codes.Internal: "INTERNAL", codes.Unavailable: "UNAVAILABLE",
	codes.DataLoss: "DATA_LOSS", codes.Unauthenticated: "UNAUTHENTICATED",
}

// CodeName returns the canonical name of a code, e.g. NOT_FOUND.
func CodeName(c codes.Code) string {
	if n, ok := codeNames[c]; ok {
		return n
	}
	return fmt.Sprintf("CODE_%d", c)
}

// ParseCode accepts NOT_FOUND, not_found, NotFound or 5.
func ParseCode(s string) (codes.Code, bool) {
	s = strings.TrimSpace(s)
	norm := strings.ToUpper(strings.ReplaceAll(s, "_", ""))
	for c, n := range codeNames {
		if strings.ReplaceAll(n, "_", "") == norm || (norm == "CANCELED" && c == codes.Canceled) {
			return c, true
		}
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n >= 0 && n <= 16 && fmt.Sprint(n) == s {
		return codes.Code(n), true
	}
	return 0, false
}

// CodeNames lists the code names in numeric order.
func CodeNames() []string {
	all := make([]int, 0, len(codeNames))
	for c := range codeNames {
		all = append(all, int(c))
	}
	sort.Ints(all)
	out := make([]string, len(all))
	for i, c := range all {
		out[i] = codeNames[codes.Code(c)]
	}
	return out
}

// HTTPStatus maps a gRPC code to the HTTP status that means the same, as
// grpc-gateway does, so INVALID_ARGUMENT counts as a rejection and INTERNAL
// as a server error.
func HTTPStatus(c codes.Code) int {
	switch c {
	case codes.OK:
		return 200
	case codes.Canceled:
		return 499
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return 400
	case codes.DeadlineExceeded:
		return 504
	case codes.NotFound:
		return 404
	case codes.AlreadyExists, codes.Aborted:
		return 409
	case codes.PermissionDenied:
		return 403
	case codes.Unauthenticated:
		return 401
	case codes.ResourceExhausted:
		return 429
	case codes.Unimplemented:
		return 501
	case codes.Unavailable:
		return 503
	}
	return 500 // UNKNOWN, INTERNAL, DATA_LOSS
}
