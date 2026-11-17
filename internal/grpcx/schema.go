package grpcx

import (
	"fmt"
	"path/filepath"
	"sort"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

// Endpoints turns services into endpoints: one per method, with the request
// message as the body schema. where returns the source location of a method,
// e.g. proto/users/v1/users.proto:14; it may be nil.
func Endpoints(services []protoreflect.ServiceDescriptor, where func(protoreflect.MethodDescriptor) string) []*model.Endpoint {
	var out []*model.Endpoint
	for _, sd := range services {
		for i := 0; i < sd.Methods().Len(); i++ {
			md := sd.Methods().Get(i)
			ep := &model.Endpoint{
				Protocol: model.ProtoGRPC,
				Method:   "GRPC",
				Path:     "/" + string(sd.FullName()) + "/" + string(md.Name()),
				Body:     MessageSchema(md.Input()),
				BodyType: "application/json",
				Responses: map[string]*model.ResponseSpec{
					"OK": {Description: string(md.Output().FullName()), Schema: MessageSchema(md.Output())},
				},
			}
			switch {
			case md.IsStreamingClient() && md.IsStreamingServer():
				ep.Stream = "bidi"
			case md.IsStreamingClient():
				ep.Stream = "client"
			case md.IsStreamingServer():
				ep.Stream = "server"
			}
			if where != nil {
				ep.Source = where(md)
			}
			out = append(out, ep)
		}
	}
	return out
}

// Location returns file:line for a descriptor, with the file mapped through
// files (import name -> path on disk) and shown relative to root.
func Location(d protoreflect.Descriptor, files map[string]string, root string) string {
	fd := d.ParentFile()
	if fd == nil {
		return ""
	}
	path := fd.Path()
	if disk, ok := files[path]; ok {
		path = disk
		if root != "" {
			if rel, err := filepath.Rel(root, disk); err == nil {
				path = filepath.ToSlash(rel)
			}
		}
	}
	loc := fd.SourceLocations().ByDescriptor(d)
	if loc.Path == nil && loc.StartLine == 0 {
		return path
	}
	return fmt.Sprintf("%s:%d", path, loc.StartLine+1)
}

// MessageSchema describes a message as a JSON object, the way protojson
// reads it: fields by their proto names, enums by name, well-known types in
// their JSON forms.
func MessageSchema(md protoreflect.MessageDescriptor) *model.Schema {
	return messageSchema(md, 0, map[protoreflect.FullName]bool{})
}

func messageSchema(md protoreflect.MessageDescriptor, depth int, seen map[protoreflect.FullName]bool) *model.Schema {
	if s := wellKnown(md); s != nil {
		return s
	}
	s := &model.Schema{Type: "object", TypeName: string(md.FullName()), Properties: map[string]*model.Schema{}}
	if seen[md.FullName()] || depth > 4 {
		return s
	}
	seen[md.FullName()] = true
	defer delete(seen, md.FullName())
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		name := string(f.Name())
		s.Properties[name] = fieldSchema(f, depth, seen)
		if f.Cardinality() == protoreflect.Required || required(f) {
			s.Required = append(s.Required, name)
		}
	}
	oneofs := md.Oneofs()
	for i := 0; i < oneofs.Len(); i++ {
		o := oneofs.Get(i)
		if o.IsSynthetic() {
			continue
		}
		var group []string
		for j := 0; j < o.Fields().Len(); j++ {
			group = append(group, string(o.Fields().Get(j).Name()))
		}
		s.Exclusive = append(s.Exclusive, group)
	}
	sort.Strings(s.Required)
	return s
}

func fieldSchema(f protoreflect.FieldDescriptor, depth int, seen map[protoreflect.FullName]bool) *model.Schema {
	if f.IsMap() {
		return &model.Schema{Type: "object", TypeName: "map<" + kindName(f.MapKey()) + ", " + kindName(f.MapValue()) + ">"}
	}
	var s *model.Schema
	switch f.Kind() {
	case protoreflect.BoolKind:
		s = &model.Schema{Type: "boolean"}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		s = &model.Schema{Type: "integer", Format: f.Kind().String()}
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		s = &model.Schema{Type: "number", Format: f.Kind().String()}
	case protoreflect.StringKind:
		s = &model.Schema{Type: "string"}
	case protoreflect.BytesKind:
		s = &model.Schema{Type: "string", Format: "byte"}
	case protoreflect.EnumKind:
		s = &model.Schema{Type: "string", TypeName: string(f.Enum().FullName())}
		vals := f.Enum().Values()
		for i := 0; i < vals.Len(); i++ {
			s.Enum = append(s.Enum, string(vals.Get(i).Name()))
		}
	case protoreflect.MessageKind, protoreflect.GroupKind:
		s = messageSchema(f.Message(), depth+1, seen)
	default:
		s = &model.Schema{}
	}
	if f.IsList() {
		return &model.Schema{Type: "array", Items: s}
	}
	return s
}

func kindName(f protoreflect.FieldDescriptor) string {
	switch {
	case f.Message() != nil:
		return string(f.Message().Name())
	case f.Enum() != nil:
		return string(f.Enum().Name())
	}
	return f.Kind().String()
}

// wellKnown returns the JSON form of the well-known types.
func wellKnown(md protoreflect.MessageDescriptor) *model.Schema {
	name := string(md.FullName())
	switch name {
	case "google.protobuf.Timestamp":
		return &model.Schema{Type: "string", Format: "date-time", TypeName: name}
	case "google.protobuf.Duration":
		return &model.Schema{Type: "string", Example: "1.5s", TypeName: name}
	case "google.protobuf.FieldMask":
		return &model.Schema{Type: "string", Example: "name", TypeName: name}
	case "google.protobuf.Struct":
		return &model.Schema{Type: "object", TypeName: name}
	case "google.protobuf.Value":
		return &model.Schema{TypeName: name}
	case "google.protobuf.ListValue":
		return &model.Schema{Type: "array", TypeName: name}
	case "google.protobuf.Empty":
		return &model.Schema{Type: "object", Properties: map[string]*model.Schema{}, TypeName: name}
	case "google.protobuf.Any":
		return &model.Schema{Type: "object", TypeName: name, Properties: map[string]*model.Schema{"@type": {Type: "string"}}}
	case "google.protobuf.StringValue", "google.protobuf.BytesValue":
		return &model.Schema{Type: "string", Nullable: true, TypeName: name}
	case "google.protobuf.BoolValue":
		return &model.Schema{Type: "boolean", Nullable: true, TypeName: name}
	case "google.protobuf.DoubleValue", "google.protobuf.FloatValue":
		return &model.Schema{Type: "number", Nullable: true, TypeName: name}
	case "google.protobuf.Int32Value", "google.protobuf.Int64Value", "google.protobuf.UInt32Value", "google.protobuf.UInt64Value":
		return &model.Schema{Type: "integer", Nullable: true, TypeName: name}
	}
	return nil
}

// required reports whether a field is marked [(google.api.field_behavior) = REQUIRED].
func required(f protoreflect.FieldDescriptor) bool {
	opts := f.Options()
	if opts == nil {
		return false
	}
	m := opts.ProtoReflect()
	if !m.IsValid() {
		return false
	}
	const fieldBehavior, requiredValue = 1052, 2
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Number() != fieldBehavior {
			return true
		}
		if fd.IsList() {
			for i := 0; i < v.List().Len(); i++ {
				if v.List().Get(i).Enum() == requiredValue {
					found = true
				}
			}
		} else if v.Enum() == requiredValue {
			found = true
		}
		return !found
	})
	if found {
		return true
	}
	b := m.GetUnknown()
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return false
		}
		b = b[n:]
		switch {
		case num == fieldBehavior && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return false
			}
			if v == requiredValue {
				return true
			}
			b = b[n:]
		case num == fieldBehavior && typ == protowire.BytesType:
			packed, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return false
			}
			for len(packed) > 0 {
				v, k := protowire.ConsumeVarint(packed)
				if k < 0 {
					break
				}
				if v == requiredValue {
					return true
				}
				packed = packed[k:]
			}
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return false
			}
			b = b[n:]
		}
	}
	return false
}
