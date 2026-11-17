package protocols

import (
	"context"
	"embed"
	"fmt"
	"io"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/bufbuild/protocompile"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

//go:embed proto
var protoFS embed.FS

// The demo builds its messages from orders.proto at startup instead of from
// generated code, so the repository has no generated files.
func compileProtos() (*protoregistry.Files, error) {
	comp := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
		Accessor: func(path string) (io.ReadCloser, error) { return protoFS.Open("proto/" + path) },
	})}
	files, err := comp.Compile(context.Background(), "orders/v1/orders.proto")
	if err != nil {
		return nil, err
	}
	reg := new(protoregistry.Files)
	var register func(fd protoreflect.FileDescriptor)
	register = func(fd protoreflect.FileDescriptor) {
		if _, err := reg.FindFileByPath(fd.Path()); err == nil {
			return
		}
		for i := 0; i < fd.Imports().Len(); i++ {
			register(fd.Imports().Get(i).FileDescriptor)
		}
		_ = reg.RegisterFile(fd)
	}
	for _, f := range files {
		register(f)
	}
	return reg, nil
}

type order struct {
	ID       int64
	Customer string
	Items    []item
	Total    int64
	Status   int32 // 1 open, 2 cancelled
}

type item struct {
	SKU   string
	Qty   int32
	Price int64
}

type ordersService struct {
	svc    protoreflect.ServiceDescriptor
	mu     sync.Mutex
	orders []*order
}

// NewGRPC returns the demo gRPC server, with server reflection on.
func NewGRPC() (*grpc.Server, error) {
	files, err := compileProtos()
	if err != nil {
		return nil, err
	}
	d, err := files.FindDescriptorByName("orders.v1.Orders")
	if err != nil {
		return nil, err
	}
	s := &ordersService{svc: d.(protoreflect.ServiceDescriptor)}
	s.orders = []*order{
		{ID: 1, Customer: "ada", Items: []item{{"BOOK-1", 2, 1500}}, Total: 3000, Status: 1},
		{ID: 2, Customer: "grace", Items: []item{{"PEN-9", 1, 250}}, Total: 250, Status: 1},
	}

	srv := grpc.NewServer(grpc.UnaryInterceptor(recoverUnary))
	desc := grpc.ServiceDesc{ServiceName: "orders.v1.Orders", HandlerType: (*any)(nil), Metadata: "orders/v1/orders.proto"}
	desc.Methods = []grpc.MethodDesc{
		s.unary("CreateOrder", s.createOrder),
		s.unary("GetOrder", s.getOrder),
		s.unary("ListOrders", s.listOrders),
		s.unary("CancelOrder", s.cancelOrder),
	}
	desc.Streams = []grpc.StreamDesc{{StreamName: "WatchOrders", ServerStreams: true, Handler: s.watchOrders}}
	srv.RegisterService(&desc, s)
	reflectionpb.RegisterServerReflectionServer(srv, reflection.NewServerV1(reflection.ServerOptions{Services: srv, DescriptorResolver: files}))
	return srv, nil
}

// recoverUnary turns a panic into INTERNAL with the stack trace in the
// message, the way many hand-written interceptors do.
func recoverUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = status.Errorf(codes.Internal, "panic: %v\n%s", r, debug.Stack())
		}
	}()
	return handler(ctx, req)
}

func (s *ordersService) method(name string) protoreflect.MethodDescriptor {
	return s.svc.Methods().ByName(protoreflect.Name(name))
}

func (s *ordersService) unary(name string, fn func(context.Context, *dynamicpb.Message) (proto.Message, error)) grpc.MethodDesc {
	md := s.method(name)
	return grpc.MethodDesc{MethodName: name, Handler: func(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
		in := dynamicpb.NewMessage(md.Input())
		if err := dec(in); err != nil {
			return nil, err
		}
		h := func(ctx context.Context, req any) (any, error) { return fn(ctx, req.(*dynamicpb.Message)) }
		if ic == nil {
			return h(ctx, in)
		}
		return ic(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/orders.v1.Orders/" + name}, h)
	}}
}

func field(m *dynamicpb.Message, name string) protoreflect.Value {
	return m.Get(m.Descriptor().Fields().ByName(protoreflect.Name(name)))
}

func (s *ordersService) toProto(o *order) *dynamicpb.Message {
	md := s.method("GetOrder").Output()
	m := dynamicpb.NewMessage(md)
	set := func(name string, v protoreflect.Value) { m.Set(md.Fields().ByName(protoreflect.Name(name)), v) }
	set("id", protoreflect.ValueOfInt64(o.ID))
	set("customer", protoreflect.ValueOfString(o.Customer))
	set("total_cents", protoreflect.ValueOfInt64(o.Total))
	set("status", protoreflect.ValueOfEnum(protoreflect.EnumNumber(o.Status)))
	items := m.Mutable(md.Fields().ByName("items")).List()
	for _, it := range o.Items {
		im := dynamicpb.NewMessage(md.Fields().ByName("items").Message())
		f := im.Descriptor().Fields()
		im.Set(f.ByName("sku"), protoreflect.ValueOfString(it.SKU))
		im.Set(f.ByName("qty"), protoreflect.ValueOfInt32(it.Qty))
		im.Set(f.ByName("price_cents"), protoreflect.ValueOfInt64(it.Price))
		items.Append(protoreflect.ValueOfMessage(im))
	}
	return m
}

func (s *ordersService) createOrder(_ context.Context, in *dynamicpb.Message) (proto.Message, error) {
	// BUG: customer is required by the .proto but never checked.
	o := &order{Customer: field(in, "customer").String(), Status: 1}
	list := field(in, "items").List()
	var total int64
	for i := 0; i < list.Len(); i++ {
		im := list.Get(i).Message()
		f := im.Descriptor().Fields()
		it := item{SKU: im.Get(f.ByName("sku")).String(), Qty: int32(im.Get(f.ByName("qty")).Int()), Price: im.Get(f.ByName("price_cents")).Int()}
		o.Items = append(o.Items, it)
		total += int64(it.Qty) * it.Price
	}
	o.Total = total
	// BUG: a total sent by the client wins over the one computed from the items.
	if t := field(in, "total_cents").Int(); t != 0 {
		o.Total = t
	}
	s.mu.Lock()
	o.ID = int64(len(s.orders) + 1)
	s.orders = append(s.orders, o)
	s.mu.Unlock()
	return s.toProto(o), nil
}

func (s *ordersService) find(id int64) *order {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.orders {
		if o.ID == id {
			return o
		}
	}
	return nil
}

func (s *ordersService) getOrder(_ context.Context, in *dynamicpb.Message) (proto.Message, error) {
	id := field(in, "id").Int()
	o := s.find(id)
	if o == nil {
		// BUG: a plain error becomes UNKNOWN; this should be NOT_FOUND.
		return nil, fmt.Errorf("sql: no rows in result set (order %d)", id)
	}
	return s.toProto(o), nil
}

func (s *ordersService) listOrders(_ context.Context, in *dynamicpb.Message) (proto.Message, error) {
	size := int(field(in, "page_size").Int())
	if size == 0 {
		size = 10
	}
	customer := field(in, "customer").String()
	s.mu.Lock()
	var matched []*order
	for _, o := range s.orders {
		if customer == "" || o.Customer == customer {
			matched = append(matched, o)
		}
	}
	s.mu.Unlock()
	sort.Slice(matched, func(i, j int) bool { return matched[i].ID < matched[j].ID })
	if size < len(matched) {
		// BUG: a negative page_size reaches the slice expression and panics.
		matched = matched[:size]
	}
	out := dynamicpb.NewMessage(s.method("ListOrders").Output())
	list := out.Mutable(out.Descriptor().Fields().ByName("orders")).List()
	for _, o := range matched {
		list.Append(protoreflect.ValueOfMessage(s.toProto(o)))
	}
	return out, nil
}

func (s *ordersService) cancelOrder(_ context.Context, in *dynamicpb.Message) (proto.Message, error) {
	o := s.find(field(in, "id").Int())
	if o == nil {
		return nil, status.Error(codes.NotFound, "no such order")
	}
	// BUG: cancelling a cancelled order succeeds again; it should be FAILED_PRECONDITION.
	s.mu.Lock()
	o.Status = 2
	s.mu.Unlock()
	return s.toProto(o), nil
}

func (s *ordersService) watchOrders(_ any, stream grpc.ServerStream) error {
	in := dynamicpb.NewMessage(s.method("WatchOrders").Input())
	if err := stream.RecvMsg(in); err != nil {
		return err
	}
	customer := field(in, "customer").String()
	s.mu.Lock()
	all := append([]*order(nil), s.orders...)
	s.mu.Unlock()
	for _, o := range all {
		if customer != "" && o.Customer != customer {
			continue
		}
		if err := stream.SendMsg(s.toProto(o)); err != nil {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	return nil
}
