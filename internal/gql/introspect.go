package gql

import (
	"encoding/json"
	"fmt"

	"github.com/vektah/gqlparser/v2/ast"
)

// IntrospectionQuery asks a server for its schema.
const IntrospectionQuery = `query ReplayHarnessIntrospection {
  __schema {
    queryType { name }
    mutationType { name }
    subscriptionType { name }
    types {
      kind name description
      fields(includeDeprecated: true) { name description args { name defaultValue type { ...T } } type { ...T } }
      inputFields { name defaultValue type { ...T } }
      enumValues(includeDeprecated: true) { name }
      possibleTypes { name }
    }
  }
}
fragment T on __Type { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name } } } } } } }`

type introType struct {
	Kind   string     `json:"kind"`
	Name   string     `json:"name"`
	OfType *introType `json:"ofType"`
}

type introValue struct {
	Name         string    `json:"name"`
	DefaultValue *string   `json:"defaultValue"`
	Type         introType `json:"type"`
}

// FromIntrospection reads the response to IntrospectionQuery.
func FromIntrospection(body []byte) (*Schema, error) {
	var resp struct {
		Data struct {
			Schema *struct {
				QueryType        *struct{ Name string } `json:"queryType"`
				MutationType     *struct{ Name string } `json:"mutationType"`
				SubscriptionType *struct{ Name string } `json:"subscriptionType"`
				Types            []struct {
					Kind        string `json:"kind"`
					Name        string `json:"name"`
					Description string `json:"description"`
					Fields      []struct {
						Name        string       `json:"name"`
						Description string       `json:"description"`
						Args        []introValue `json:"args"`
						Type        introType    `json:"type"`
					} `json:"fields"`
					InputFields []introValue            `json:"inputFields"`
					EnumValues  []struct{ Name string } `json:"enumValues"`
					Possible    []struct{ Name string } `json:"possibleTypes"`
				} `json:"types"`
			} `json:"__schema"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("not a GraphQL response: %v", err)
	}
	if resp.Data.Schema == nil {
		if len(resp.Errors) > 0 {
			return nil, fmt.Errorf("introspection failed: %s", resp.Errors[0].Message)
		}
		return nil, fmt.Errorf("introspection returned no schema")
	}
	sc := resp.Data.Schema
	s := &Schema{Types: map[string]*ast.Definition{}, where: map[string]string{}}
	if sc.QueryType != nil {
		s.Query = sc.QueryType.Name
	}
	if sc.MutationType != nil {
		s.Mutation = sc.MutationType.Name
	}
	if sc.SubscriptionType != nil {
		s.Subscription = sc.SubscriptionType.Name
	}
	for _, t := range sc.Types {
		d := &ast.Definition{Kind: ast.DefinitionKind(t.Kind), Name: t.Name, Description: t.Description}
		for _, f := range t.Fields {
			fd := &ast.FieldDefinition{Name: f.Name, Description: f.Description, Type: toType(f.Type)}
			for _, a := range f.Args {
				fd.Arguments = append(fd.Arguments, &ast.ArgumentDefinition{Name: a.Name, Type: toType(a.Type), DefaultValue: defaultValue(a.DefaultValue)})
			}
			d.Fields = append(d.Fields, fd)
		}
		for _, f := range t.InputFields {
			d.Fields = append(d.Fields, &ast.FieldDefinition{Name: f.Name, Type: toType(f.Type), DefaultValue: defaultValue(f.DefaultValue)})
		}
		for _, v := range t.EnumValues {
			d.EnumValues = append(d.EnumValues, &ast.EnumValueDefinition{Name: v.Name})
		}
		for _, p := range t.Possible {
			d.Types = append(d.Types, p.Name)
		}
		s.Types[t.Name] = d
	}
	return s, nil
}

func defaultValue(v *string) *ast.Value {
	if v == nil {
		return nil
	}
	return &ast.Value{Raw: *v}
}

func toType(t introType) *ast.Type {
	switch t.Kind {
	case "NON_NULL":
		if t.OfType == nil {
			return &ast.Type{NamedType: "String", NonNull: true}
		}
		inner := toType(*t.OfType)
		inner.NonNull = true
		return inner
	case "LIST":
		if t.OfType == nil {
			return &ast.Type{Elem: &ast.Type{NamedType: "String"}}
		}
		return &ast.Type{Elem: toType(*t.OfType)}
	}
	return &ast.Type{NamedType: t.Name}
}
