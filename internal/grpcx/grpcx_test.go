package grpcx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestCodesAndMethods(t *testing.T) {
	for in, want := range map[string]codes.Code{"NOT_FOUND": codes.NotFound, "not_found": codes.NotFound, "NotFound": codes.NotFound,
		"5": codes.NotFound, "CANCELED": codes.Canceled, "CANCELLED": codes.Canceled, "OK": codes.OK, "16": codes.Unauthenticated} {
		if got, ok := ParseCode(in); !ok || got != want {
			t.Errorf("ParseCode(%q) = %v %v", in, got, ok)
		}
	}
	for _, bad := range []string{"nope", "17", "-1", ""} {
		if _, ok := ParseCode(bad); ok {
			t.Errorf("ParseCode(%q) accepted", bad)
		}
	}
	for _, in := range []string{"/users.v1.Users/GetUser", "users.v1.Users/GetUser", "users.v1.Users.GetUser"} {
		if s, m, err := SplitMethod(in); err != nil || s != "users.v1.Users" || m != "GetUser" {
			t.Errorf("SplitMethod(%q) = %q %q %v", in, s, m, err)
		}
	}
	if _, _, err := SplitMethod("GetUser"); err == nil {
		t.Error("a bare method name should be refused")
	}
	if HTTPStatus(codes.InvalidArgument) != 400 || HTTPStatus(codes.Internal) != 500 || HTTPStatus(codes.Unknown) != 500 || HTTPStatus(codes.OK) != 200 {
		t.Error("HTTPStatus mapping")
	}
	if addr, tls, err := ParseTarget("grpcs://localhost:443"); err != nil || !tls || addr != "localhost:443" {
		t.Errorf("ParseTarget = %q %v %v", addr, tls, err)
	}
	if _, _, err := ParseTarget("localhost"); err == nil {
		t.Error("a target without a port should be refused")
	}
}

func TestCompile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("proto/shop/v1/common.proto", `syntax = "proto3";
package shop.v1;
message Money { int64 cents = 1; string currency = 2; }`)
	write("proto/shop/v1/shop.proto", `syntax = "proto3";
package shop.v1;
import "shop/v1/common.proto";
import "google/api/annotations.proto";
import "google/api/field_behavior.proto";
import "google/protobuf/timestamp.proto";
service Shop {
  rpc Buy(BuyRequest) returns (Receipt) { option (google.api.http) = { post: "/v1/buy" body: "*" }; }
  rpc Watch(BuyRequest) returns (stream Receipt);
}
message BuyRequest {
  string sku = 1 [(google.api.field_behavior) = REQUIRED];
  oneof payment { string card = 2; string voucher = 3; }
  optional int32 qty = 4;
  map<string, string> labels = 5;
}
message Receipt { Money total = 1; google.protobuf.Timestamp at = 2; }`)
	write("broken/bad.proto", `syntax = "proto3"; import "missing/dep.proto"; message X { dep.Y y = 1; }`)

	set := Compile([]string{dir})
	if len(set.Notes) != 1 || !strings.Contains(set.Notes[0], "bad.proto") {
		t.Errorf("notes = %v", set.Notes)
	}
	services := ServicesIn(set.Files, set.Own)
	if len(services) != 1 || services[0].FullName() != "shop.v1.Shop" {
		t.Fatalf("services = %v", services)
	}
	eps := Endpoints(services, func(md protoreflect.MethodDescriptor) string { return Location(md, set.Own, dir) })
	if len(eps) != 2 || eps[0].Path != "/shop.v1.Shop/Buy" || eps[1].Stream != "server" {
		t.Fatalf("endpoints = %+v", eps)
	}
	if eps[0].Source != "proto/shop/v1/shop.proto:8" {
		t.Errorf("source = %s", eps[0].Source)
	}
	body := eps[0].Body
	if !body.IsRequired("sku") || body.IsRequired("qty") || len(body.Exclusive) != 1 || len(body.Exclusive[0]) != 2 {
		t.Errorf("request schema: required %v, oneofs %v", body.Required, body.Exclusive)
	}
	if body.Properties["labels"].Type != "object" || body.Properties["qty"].Format != "int32" {
		t.Errorf("fields: %+v %+v", body.Properties["labels"], body.Properties["qty"])
	}
	resp := eps[0].Responses["OK"].Schema
	if resp.Properties["at"].Format != "date-time" || resp.Properties["total"].TypeName != "shop.v1.Money" {
		t.Errorf("response schema: %+v", resp.Properties)
	}
}
