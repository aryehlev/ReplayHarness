package grpcx

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	rpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	// Register the well-known types and the common error details, so they
	// resolve even when the server does not send them.
	_ "google.golang.org/genproto/googleapis/rpc/errdetails"
	_ "google.golang.org/protobuf/types/known/anypb"
	_ "google.golang.org/protobuf/types/known/durationpb"
	_ "google.golang.org/protobuf/types/known/emptypb"
	_ "google.golang.org/protobuf/types/known/fieldmaskpb"
	_ "google.golang.org/protobuf/types/known/structpb"
	_ "google.golang.org/protobuf/types/known/timestamppb"
	_ "google.golang.org/protobuf/types/known/wrapperspb"
)

var errNoReflection = errors.New("the server does not support reflection")

// reflector asks the server for its descriptors through the reflection
// service (v1, falling back to v1alpha, which is wire-compatible).
type reflector struct {
	conn  *grpc.ClientConn
	alpha bool
	fdps  map[string]*descriptorpb.FileDescriptorProto
	files *protoregistry.Files
}

const (
	reflectV1      = "/grpc.reflection.v1.ServerReflection/ServerReflectionInfo"
	reflectV1Alpha = "/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo"
)

func (r *reflector) call(ctx context.Context, reqs []*rpb.ServerReflectionRequest) ([]*rpb.ServerReflectionResponse, error) {
	methods := []string{reflectV1, reflectV1Alpha}
	if r.alpha {
		methods = methods[1:]
	}
	var lastErr error
	for _, m := range methods {
		out, err := r.callOne(ctx, m, reqs)
		if err == nil {
			r.alpha = m == reflectV1Alpha
			return out, nil
		}
		if status.Code(err) != codes.Unimplemented {
			return nil, err
		}
		lastErr = errNoReflection
	}
	return nil, lastErr
}

func (r *reflector) callOne(ctx context.Context, method string, reqs []*rpb.ServerReflectionRequest) ([]*rpb.ServerReflectionResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	s, err := r.conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, method)
	if err != nil {
		return nil, err
	}
	out := make([]*rpb.ServerReflectionResponse, 0, len(reqs))
	for _, q := range reqs {
		resp := new(rpb.ServerReflectionResponse)
		if err := s.SendMsg(q); err != nil {
			if rerr := s.RecvMsg(resp); rerr != nil {
				return nil, rerr
			}
			return nil, err
		}
		if err := s.RecvMsg(resp); err != nil {
			return nil, err
		}
		out = append(out, resp)
	}
	_ = s.CloseSend()
	return out, nil
}

// listServices returns the service names the server reports, without the
// reflection services themselves.
func (r *reflector) listServices(ctx context.Context) ([]string, error) {
	resps, err := r.call(ctx, []*rpb.ServerReflectionRequest{{MessageRequest: &rpb.ServerReflectionRequest_ListServices{ListServices: "*"}}})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range resps[0].GetListServicesResponse().GetService() {
		if !strings.HasPrefix(s.GetName(), "grpc.reflection.") {
			out = append(out, s.GetName())
		}
	}
	sort.Strings(out)
	return out, nil
}

// fileContaining fetches the file that defines symbol, and its imports.
func (r *reflector) fileContaining(ctx context.Context, symbol string) (*protoregistry.Files, error) {
	if r.files != nil {
		if _, err := r.files.FindDescriptorByName(protoreflect.FullName(symbol)); err == nil {
			return r.files, nil
		}
	}
	if r.fdps == nil {
		r.fdps = map[string]*descriptorpb.FileDescriptorProto{}
	}
	resps, err := r.call(ctx, []*rpb.ServerReflectionRequest{{MessageRequest: &rpb.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: symbol}}})
	if err != nil {
		return nil, err
	}
	if err := r.add(resps); err != nil {
		return nil, err
	}
	for round := 0; round < 10; round++ {
		var missing []*rpb.ServerReflectionRequest
		for _, fd := range r.fdps {
			for _, dep := range fd.GetDependency() {
				if _, ok := r.fdps[dep]; ok {
					continue
				}
				if g, err := protoregistry.GlobalFiles.FindFileByPath(dep); err == nil {
					r.fdps[dep] = protodesc.ToFileDescriptorProto(g)
					continue
				}
				missing = append(missing, &rpb.ServerReflectionRequest{MessageRequest: &rpb.ServerReflectionRequest_FileByFilename{FileByFilename: dep}})
			}
		}
		if len(missing) == 0 {
			break
		}
		resps, err := r.call(ctx, missing)
		if err != nil {
			return nil, err
		}
		if err := r.add(resps); err != nil {
			return nil, err
		}
	}
	set := &descriptorpb.FileDescriptorSet{}
	names := make([]string, 0, len(r.fdps))
	for n := range r.fdps {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		set.File = append(set.File, r.fdps[n])
	}
	files, err := protodesc.FileOptions{AllowUnresolvable: true}.NewFiles(set)
	if err != nil {
		return nil, fmt.Errorf("the descriptors from reflection do not link: %w", err)
	}
	r.files = files
	return files, nil
}

func (r *reflector) add(resps []*rpb.ServerReflectionResponse) error {
	for _, resp := range resps {
		if e := resp.GetErrorResponse(); e != nil {
			return fmt.Errorf("%s", e.GetErrorMessage())
		}
		for _, b := range resp.GetFileDescriptorResponse().GetFileDescriptorProto() {
			fd := new(descriptorpb.FileDescriptorProto)
			if err := proto.Unmarshal(b, fd); err != nil {
				return err
			}
			r.fdps[fd.GetName()] = fd
		}
	}
	return nil
}

// Services returns every service the server or the .proto files define:
// reflection first, then the .proto files for anything reflection did not
// list. source reports where each one came from.
func (c *Client) Services(ctx context.Context) (services []protoreflect.ServiceDescriptor, source string, notes []string, err error) {
	c.mu.Lock()
	c.loadLocal()
	local, own, localNotes := c.local, c.own, c.notes
	names, rerr := c.reflect.listServices(ctx)
	c.mu.Unlock()
	notes = append(notes, localNotes...)
	seen := map[string]bool{}
	if rerr == nil {
		for _, n := range names {
			sd, err := c.Service(ctx, n)
			if err != nil {
				notes = append(notes, err.Error())
				continue
			}
			seen[n] = true
			services = append(services, sd)
		}
		source = "gRPC server reflection on " + c.addr
	} else if !errors.Is(rerr, errNoReflection) {
		notes = append(notes, "gRPC reflection on "+c.addr+" failed: "+rerr.Error())
	}
	if local != nil {
		for _, sd := range ServicesIn(local, own) {
			if !seen[string(sd.FullName())] {
				services = append(services, sd)
			}
		}
	}
	if len(services) == 0 && rerr != nil {
		return nil, "", notes, rerr
	}
	return services, source, notes, nil
}

// dynamicResolver finds message types in the .proto files and in what
// reflection returned, then in the types compiled into ReplayHarness.
type dynamicResolver struct{ c *Client }

func (r dynamicResolver) find(name protoreflect.FullName) protoreflect.MessageDescriptor {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	for _, f := range []*protoregistry.Files{r.c.local, r.c.reflect.files} {
		if f == nil {
			continue
		}
		if d, err := f.FindDescriptorByName(name); err == nil {
			if md, ok := d.(protoreflect.MessageDescriptor); ok {
				return md
			}
		}
	}
	return nil
}

func (r dynamicResolver) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error) {
	if mt, err := protoregistry.GlobalTypes.FindMessageByName(name); err == nil {
		return mt, nil
	}
	if md := r.find(name); md != nil {
		return dynamicpb.NewMessageType(md), nil
	}
	return nil, protoregistry.NotFound
}

func (r dynamicResolver) FindMessageByURL(url string) (protoreflect.MessageType, error) {
	if i := strings.LastIndex(url, "/"); i >= 0 {
		url = url[i+1:]
	}
	return r.FindMessageByName(protoreflect.FullName(url))
}

func (r dynamicResolver) FindExtensionByName(field protoreflect.FullName) (protoreflect.ExtensionType, error) {
	return protoregistry.GlobalTypes.FindExtensionByName(field)
}

func (r dynamicResolver) FindExtensionByNumber(message protoreflect.FullName, field protoreflect.FieldNumber) (protoreflect.ExtensionType, error) {
	return protoregistry.GlobalTypes.FindExtensionByNumber(message, field)
}
