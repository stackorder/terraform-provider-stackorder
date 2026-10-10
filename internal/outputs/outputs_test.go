package outputs_test

import (
	"context"
	"encoding/json"
	"maps"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/stackorder/stackorder/api/v1"

	"github.com/stackorder/terraform-provider-stackorder/internal/outputs"
)

const canary = "acme-canary-secret"

func output(typ, value string) v1.StackOutput {
	return v1.StackOutput{Type: json.RawMessage(typ), Value: json.RawMessage(value)}
}

func str(s string) basetypes.StringValue { return types.StringValue(s) }

func num(f float64) basetypes.NumberValue { return types.NumberValue(big.NewFloat(f)) }

var (
	portType = map[string]attr.Type{"id": types.StringType, "port": types.NumberType}

	subnetType = types.ObjectType{AttrTypes: map[string]attr.Type{"id": types.StringType, "zone": types.StringType}}
)

func assertSameValue(t *testing.T, want, got types.Dynamic) {
	t.Helper()
	ctx := context.Background()
	assert.Truef(t, want.Equal(got), "want %s, got %s", want, got)
	wantTF, err := want.ToTerraformValue(ctx)
	require.NoError(t, err)
	gotTF, err := got.ToTerraformValue(ctx)
	require.NoError(t, err)
	assert.Truef(t, wantTF.Equal(gotTF), "want %s, got %s", wantTF, gotTF)
}

func TestValue(t *testing.T) {
	tests := []struct {
		name  string
		typ   string
		value string
		want  types.Dynamic
	}{
		{name: "string", typ: `"string"`, value: `"vpc-0a1b2c3d4e5f67890"`, want: types.DynamicValue(str("vpc-0a1b2c3d4e5f67890"))},
		{name: "empty string", typ: `"string"`, value: `""`, want: types.DynamicValue(str(""))},
		{name: "number", typ: `"number"`, value: `3`, want: types.DynamicValue(num(3))},
		{name: "negative fraction", typ: `"number"`, value: `-2.5`, want: types.DynamicValue(num(-2.5))},
		{name: "bool", typ: `"bool"`, value: `true`, want: types.DynamicValue(types.BoolValue(true))},
		{name: "string from a number", typ: `"string"`, value: `12`, want: types.DynamicValue(str("12"))},
		{name: "string from a bool", typ: `"string"`, value: `true`, want: types.DynamicValue(str("true"))},
		{name: "number from a string", typ: `"number"`, value: `"12.5"`, want: types.DynamicValue(num(12.5))},
		{name: "bool from a string", typ: `"bool"`, value: `"false"`, want: types.DynamicValue(types.BoolValue(false))},
		{
			name: "list", typ: `["list","string"]`, value: `["subnet-0a1b","subnet-0b2c"]`,
			want: types.DynamicValue(types.ListValueMust(types.StringType, []attr.Value{str("subnet-0a1b"), str("subnet-0b2c")})),
		},
		{
			name: "empty list", typ: `["list","string"]`, value: `[]`,
			want: types.DynamicValue(types.ListValueMust(types.StringType, []attr.Value{})),
		},
		{
			name: "set", typ: `["set","number"]`, value: `[1,2]`,
			want: types.DynamicValue(types.SetValueMust(types.NumberType, []attr.Value{num(1), num(2)})),
		},
		{
			name: "set that repeats an element", typ: `["set","string"]`, value: `["acme","acme"]`,
			want: types.DynamicValue(types.SetValueMust(types.StringType, []attr.Value{str("acme"), str("acme")})),
		},
		{
			name: "map", typ: `["map","bool"]`, value: `{"blue":true,"green":false}`,
			want: types.DynamicValue(types.MapValueMust(types.BoolType, map[string]attr.Value{
				"blue": types.BoolValue(true), "green": types.BoolValue(false),
			})),
		},
		{
			name: "empty map", typ: `["map","string"]`, value: `{}`,
			want: types.DynamicValue(types.MapValueMust(types.StringType, map[string]attr.Value{})),
		},
		{
			name: "tuple", typ: `["tuple",["string","number","bool"]]`, value: `["eu-west-1",3,false]`,
			want: types.DynamicValue(types.TupleValueMust(
				[]attr.Type{types.StringType, types.NumberType, types.BoolType},
				[]attr.Value{str("eu-west-1"), num(3), types.BoolValue(false)},
			)),
		},
		{
			name: "empty tuple", typ: `["tuple",[]]`, value: `[]`,
			want: types.DynamicValue(types.TupleValueMust([]attr.Type{}, []attr.Value{})),
		},
		{
			name: "object", typ: `["object",{"id":"string","port":"number"}]`, value: `{"id":"sg-0a1b","port":443}`,
			want: types.DynamicValue(types.ObjectValueMust(portType, map[string]attr.Value{"id": str("sg-0a1b"), "port": num(443)})),
		},
		{
			name: "empty object", typ: `["object",{}]`, value: `{}`,
			want: types.DynamicValue(types.ObjectValueMust(map[string]attr.Type{}, map[string]attr.Value{})),
		},
		{
			name: "object attribute absent from the value", typ: `["object",{"id":"string","port":"number"}]`, value: `{"id":"sg-0a1b"}`,
			want: types.DynamicValue(types.ObjectValueMust(portType, map[string]attr.Value{"id": str("sg-0a1b"), "port": types.NumberNull()})),
		},
		{
			name: "object with optional attributes", typ: `["object",{"id":"string","port":"number"},["port"]]`, value: `{"id":"sg-0a1b"}`,
			want: types.DynamicValue(types.ObjectValueMust(portType, map[string]attr.Value{"id": str("sg-0a1b"), "port": types.NumberNull()})),
		},
		{
			name: "list of objects with optional attributes", typ: `["list",["object",{"id":"string","port":"number"},["port"]]]`,
			value: `[{"id":"sg-0a1b","port":443},{"id":"sg-0b2c"}]`,
			want: types.DynamicValue(types.ListValueMust(types.ObjectType{AttrTypes: portType}, []attr.Value{
				types.ObjectValueMust(portType, map[string]attr.Value{"id": str("sg-0a1b"), "port": num(443)}),
				types.ObjectValueMust(portType, map[string]attr.Value{"id": str("sg-0b2c"), "port": types.NumberNull()}),
			})),
		},
		{
			name: "nested",
			typ: `["object",{
				"subnets":["list",["object",{"id":"string","zone":"string"}]],
				"routes":["map",["set","string"]],
				"pair":["tuple",["string",["object",{"id":"string","port":"number"},["port"]]]]
			}]`,
			value: `{
				"subnets":[{"id":"subnet-0a1b","zone":"eu-west-1a"},{"id":"subnet-0b2c","zone":"eu-west-1b"}],
				"routes":{"private":["10.0.1.0/24","10.0.2.0/24"],"public":[]},
				"pair":["acme",{"id":"sg-0a1b"}]
			}`,
			want: types.DynamicValue(types.ObjectValueMust(
				map[string]attr.Type{
					"subnets": types.ListType{ElemType: subnetType},
					"routes":  types.MapType{ElemType: types.SetType{ElemType: types.StringType}},
					"pair":    types.TupleType{ElemTypes: []attr.Type{types.StringType, types.ObjectType{AttrTypes: portType}}},
				},
				map[string]attr.Value{
					"subnets": types.ListValueMust(subnetType, []attr.Value{
						types.ObjectValueMust(subnetType.AttrTypes, map[string]attr.Value{"id": str("subnet-0a1b"), "zone": str("eu-west-1a")}),
						types.ObjectValueMust(subnetType.AttrTypes, map[string]attr.Value{"id": str("subnet-0b2c"), "zone": str("eu-west-1b")}),
					}),
					"routes": types.MapValueMust(types.SetType{ElemType: types.StringType}, map[string]attr.Value{
						"private": types.SetValueMust(types.StringType, []attr.Value{str("10.0.1.0/24"), str("10.0.2.0/24")}),
						"public":  types.SetValueMust(types.StringType, []attr.Value{}),
					}),
					"pair": types.TupleValueMust(
						[]attr.Type{types.StringType, types.ObjectType{AttrTypes: portType}},
						[]attr.Value{str("acme"), types.ObjectValueMust(portType, map[string]attr.Value{"id": str("sg-0a1b"), "port": types.NumberNull()})},
					),
				},
			)),
		},
		{name: "dynamic", typ: `"dynamic"`, value: `{"type":"string","value":"acme"}`, want: types.DynamicValue(str("acme"))},
		{
			name: "dynamic object", typ: `"dynamic"`, value: `{"value":{"id":"sg-0a1b","port":443},"type":["object",{"id":"string","port":"number"}]}`,
			want: types.DynamicValue(types.ObjectValueMust(portType, map[string]attr.Value{"id": str("sg-0a1b"), "port": num(443)})),
		},
		{
			name: "dynamic attributes", typ: `["object",{"name":"dynamic","owner":"dynamic"}]`, value: `{"name":{"type":"string","value":"acme"},"owner":null}`,
			want: types.DynamicValue(types.ObjectValueMust(
				map[string]attr.Type{"name": types.StringType, "owner": types.DynamicType},
				map[string]attr.Value{"name": str("acme"), "owner": types.DynamicNull()},
			)),
		},
		{
			name: "list of dynamic", typ: `["list","dynamic"]`, value: `[{"type":"number","value":1},{"type":"number","value":2}]`,
			want: types.DynamicValue(types.ListValueMust(types.NumberType, []attr.Value{num(1), num(2)})),
		},
		{
			name: "map of dynamic", typ: `["map","dynamic"]`, value: `{"blue":{"type":"bool","value":true}}`,
			want: types.DynamicValue(types.MapValueMust(types.BoolType, map[string]attr.Value{"blue": types.BoolValue(true)})),
		},
		{
			name: "empty list of dynamic", typ: `["list","dynamic"]`, value: `[]`,
			want: types.DynamicValue(types.ListValueMust(types.DynamicType, []attr.Value{})),
		},
		{
			name: "list of null dynamic", typ: `["list","dynamic"]`, value: `[null]`,
			want: types.DynamicValue(types.ListValueMust(types.DynamicType, []attr.Value{types.DynamicNull()})),
		},
		{
			name: "list of dynamic with a null element", typ: `["list","dynamic"]`, value: `[null,{"type":"string","value":"acme"}]`,
			want: types.DynamicValue(types.ListValueMust(types.StringType, []attr.Value{types.StringNull(), str("acme")})),
		},
		{
			name: "set of dynamic with a null element", typ: `["set","dynamic"]`, value: `[{"type":"number","value":1},null]`,
			want: types.DynamicValue(types.SetValueMust(types.NumberType, []attr.Value{num(1), types.NumberNull()})),
		},
		{
			name: "map of dynamic with a null element", typ: `["map","dynamic"]`, value: `{"blue":null,"green":{"type":"bool","value":true}}`,
			want: types.DynamicValue(types.MapValueMust(types.BoolType, map[string]attr.Value{"blue": types.BoolNull(), "green": types.BoolValue(true)})),
		},
		{name: "dynamic typed null", typ: `"dynamic"`, value: `{"type":"string","value":null}`, want: types.DynamicValue(types.StringNull())},
		{name: "dynamic in dynamic", typ: `"dynamic"`, value: `{"type":"dynamic","value":{"type":"bool","value":true}}`, want: types.DynamicValue(types.BoolValue(true))},
		{
			name: "dynamic object with optional attributes", typ: `"dynamic"`,
			value: `{"type":["object",{"id":"string","port":"number"},["port"]],"value":{"id":"sg-0a1b"}}`,
			want:  types.DynamicValue(types.ObjectValueMust(portType, map[string]attr.Value{"id": str("sg-0a1b"), "port": types.NumberNull()})),
		},
		{
			name: "dynamic list of objects with optional attributes", typ: `"dynamic"`,
			value: `{"type":["list",["object",{"id":"string","port":"number"},["port"]]],"value":[{"id":"sg-0a1b","port":443},{"id":"sg-0b2c"}]}`,
			want: types.DynamicValue(types.ListValueMust(types.ObjectType{AttrTypes: portType}, []attr.Value{
				types.ObjectValueMust(portType, map[string]attr.Value{"id": str("sg-0a1b"), "port": num(443)}),
				types.ObjectValueMust(portType, map[string]attr.Value{"id": str("sg-0b2c"), "port": types.NumberNull()}),
			})),
		},
		{
			name: "dynamic null object with optional attributes", typ: `"dynamic"`,
			value: `{"type":["object",{"id":"string","port":"number"},["port"]],"value":null}`,
			want:  types.DynamicValue(types.ObjectNull(portType)),
		},
		{
			name: "dynamic attribute holding a null object with optional attributes", typ: `["object",{"rule":"dynamic"}]`,
			value: `{"rule":{"type":["object",{"id":"string","port":"number"},["port"]],"value":null}}`,
			want: types.DynamicValue(types.ObjectValueMust(
				map[string]attr.Type{"rule": types.ObjectType{AttrTypes: portType}},
				map[string]attr.Value{"rule": types.ObjectNull(portType)},
			)),
		},
		{
			name: "dynamic empty list of objects with optional attributes", typ: `"dynamic"`,
			value: `{"type":["list",["object",{"id":"string","port":"number"},["port"]]],"value":[]}`,
			want:  types.DynamicValue(types.ListValueMust(types.ObjectType{AttrTypes: portType}, []attr.Value{})),
		},
		{
			name: "dynamic empty map of objects with optional attributes", typ: `"dynamic"`,
			value: `{"type":["map",["object",{"id":"string","port":"number"},["port"]]],"value":{}}`,
			want:  types.DynamicValue(types.MapValueMust(types.ObjectType{AttrTypes: portType}, map[string]attr.Value{})),
		},
		{
			name: "dynamic null list of objects with optional attributes", typ: `"dynamic"`,
			value: `{"type":["list",["object",{"id":"string","port":"number"},["port"]]],"value":null}`,
			want:  types.DynamicValue(types.ListNull(types.ObjectType{AttrTypes: portType})),
		},
		{name: "null string", typ: `"string"`, value: `null`, want: types.DynamicValue(types.StringNull())},
		{name: "null number", typ: `"number"`, value: `null`, want: types.DynamicValue(types.NumberNull())},
		{name: "null bool", typ: `"bool"`, value: `null`, want: types.DynamicValue(types.BoolNull())},
		{name: "null list", typ: `["list","string"]`, value: `null`, want: types.DynamicValue(types.ListNull(types.StringType))},
		{name: "null set", typ: `["set","number"]`, value: `null`, want: types.DynamicValue(types.SetNull(types.NumberType))},
		{name: "null map", typ: `["map","bool"]`, value: `null`, want: types.DynamicValue(types.MapNull(types.BoolType))},
		{
			name: "null tuple", typ: `["tuple",["string","number"]]`, value: `null`,
			want: types.DynamicValue(types.TupleNull([]attr.Type{types.StringType, types.NumberType})),
		},
		{
			name: "null object", typ: `["object",{"id":"string","port":"number"}]`, value: `null`,
			want: types.DynamicValue(types.ObjectNull(portType)),
		},
		{
			name: "null object with optional attributes", typ: `["object",{"id":"string","port":"number"},["port"]]`, value: `null`,
			want: types.DynamicValue(types.ObjectNull(portType)),
		},
		{
			name: "null list of objects with optional attributes", typ: `["list",["object",{"id":"string","port":"number"},["port"]]]`, value: `null`,
			want: types.DynamicValue(types.ListNull(types.ObjectType{AttrTypes: portType})),
		},
		{
			name: "null element of an object type with optional attributes", typ: `["list",["object",{"id":"string","port":"number"},["port"]]]`, value: `[null]`,
			want: types.DynamicValue(types.ListValueMust(types.ObjectType{AttrTypes: portType}, []attr.Value{types.ObjectNull(portType)})),
		},
		{
			name: "null attribute of an object type with optional attributes", typ: `["object",{"rule":["object",{"id":"string","port":"number"},["port"]]}]`, value: `{"rule":null}`,
			want: types.DynamicValue(types.ObjectValueMust(
				map[string]attr.Type{"rule": types.ObjectType{AttrTypes: portType}},
				map[string]attr.Value{"rule": types.ObjectNull(portType)},
			)),
		},
		{name: "null dynamic", typ: `"dynamic"`, value: `null`, want: types.DynamicNull()},
		{
			name: "null elements", typ: `["list","string"]`, value: `["acme",null]`,
			want: types.DynamicValue(types.ListValueMust(types.StringType, []attr.Value{str("acme"), types.StringNull()})),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := outputs.Value(output(tt.typ, tt.value))
			require.NoError(t, err)
			assertSameValue(t, tt.want, got)
		})
	}
}

func TestValueKeepsNumberPrecision(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: `0.1`, want: "0.1"},
		{in: `1e20`, want: "100000000000000000000"},
		{in: `9007199254740993`, want: "9007199254740993"},
		{in: `-123456789012345678901234567890.123456789`, want: "-123456789012345678901234567890.123456789"},
		{in: `1e-30`, want: "0.000000000000000000000000000001"},
		{in: `3.14159265358979323846264338327950288419716939937510`, want: "3.1415926535897932384626433832795028841971693993751"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := outputs.Value(output(`"number"`, tt.in))
			require.NoError(t, err)
			n, ok := got.UnderlyingValue().(basetypes.NumberValue)
			require.Truef(t, ok, "want a number, got %T", got.UnderlyingValue())
			assert.Equal(t, tt.want, n.ValueBigFloat().Text('f', -1))

			list, err := outputs.Value(output(`["list","number"]`, "["+tt.in+"]"))
			require.NoError(t, err)
			l, ok := list.UnderlyingValue().(basetypes.ListValue)
			require.Truef(t, ok, "want a list, got %T", list.UnderlyingValue())
			require.Len(t, l.Elements(), 1)
			assert.Truef(t, n.Equal(l.Elements()[0]), "want %s, got %s", n, l.Elements()[0])
		})
	}
}

func TestValueRejectsMalformedOutputs(t *testing.T) {
	tests := []struct {
		name    string
		out     v1.StackOutput
		wantErr error
		wantMsg string
	}{
		{name: "missing type", out: v1.StackOutput{Value: json.RawMessage(`"` + canary + `"`)}, wantErr: outputs.ErrMalformedType},
		{name: "type not JSON", out: output(`string`, `"`+canary+`"`), wantErr: outputs.ErrMalformedType},
		{name: "unknown primitive", out: output(`"text"`, `"`+canary+`"`), wantErr: outputs.ErrMalformedType, wantMsg: `"text"`},
		{name: "unknown kind", out: output(`["vector","string"]`, `["`+canary+`"]`), wantErr: outputs.ErrMalformedType},
		{name: "extra data in a type", out: output(`["list","string","number"]`, `["`+canary+`"]`), wantErr: outputs.ErrMalformedType},
		{name: "null type", out: output(`null`, `"`+canary+`"`), wantErr: outputs.ErrMalformedType},
		{name: "kind not a string", out: output(`[1,"string"]`, `["`+canary+`"]`), wantErr: outputs.ErrMalformedType},
		{name: "list type without its element type", out: output(`["list"]`, `["`+canary+`"]`), wantErr: outputs.ErrMalformedType},
		{name: "unknown element type", out: output(`["map","text"]`, `{"a":"`+canary+`"}`), wantErr: outputs.ErrMalformedType, wantMsg: `"text"`},
		{name: "tuple type without element types", out: output(`["tuple",null]`, `["`+canary+`"]`), wantErr: outputs.ErrMalformedType},
		{name: "object type without attributes", out: output(`["object"]`, `{"id":"`+canary+`"}`), wantErr: outputs.ErrMalformedType},
		{name: "object type with null attributes", out: output(`["object",null]`, `{"id":"`+canary+`"}`), wantErr: outputs.ErrMalformedType},
		{name: "object type with extra data", out: output(`["object",{"id":"string"},["id"],"x"]`, `{"id":"`+canary+`"}`), wantErr: outputs.ErrMalformedType},
		{name: "optional attributes not an array", out: output(`["object",{"id":"string"},"id"]`, `{"id":"`+canary+`"}`), wantErr: outputs.ErrMalformedType},
		{
			name: "optional attribute not in the object type", out: output(`["object",{"id":"string"},["port"]]`, `{"id":"`+canary+`"}`),
			wantErr: outputs.ErrMalformedType, wantMsg: `"port"`,
		},
		{name: "missing value", out: v1.StackOutput{Type: json.RawMessage(`"string"`)}, wantErr: outputs.ErrMalformedValue},
		{name: "value not JSON", out: output(`"string"`, canary), wantErr: outputs.ErrMalformedValue},
		{name: "data after the value", out: output(`"string"`, `"`+canary+`" "acme"`), wantErr: outputs.ErrMalformedValue},
		{name: "number from text", out: output(`"number"`, `"`+canary+`"`), wantErr: outputs.ErrMalformedValue},
		{name: "bool from text", out: output(`"bool"`, `"`+canary+`"`), wantErr: outputs.ErrMalformedValue},
		{name: "string from an object", out: output(`"string"`, `{"`+canary+`":1}`), wantErr: outputs.ErrMalformedValue},
		{name: "list from an object", out: output(`["list","string"]`, `{"id":"`+canary+`"}`), wantErr: outputs.ErrMalformedValue},
		{name: "undeclared attribute", out: output(`["object",{"id":"string"}]`, `{"id":"acme","`+canary+`":"x"}`), wantErr: outputs.ErrMalformedValue},
		{name: "tuple too long", out: output(`["tuple",["string"]]`, `["`+canary+`","acme"]`), wantErr: outputs.ErrMalformedValue},
		{name: "tuple too short", out: output(`["tuple",["string","string"]]`, `["`+canary+`"]`), wantErr: outputs.ErrMalformedValue},
		{
			name: "list of dynamic elements of different types", out: output(`["list","dynamic"]`, `[{"type":"string","value":"`+canary+`"},{"type":"number","value":1}]`),
			wantErr: outputs.ErrMalformedValue,
		},
		{
			name: "map of dynamic elements of different types", out: output(`["map","dynamic"]`, `{"a":{"type":"string","value":"`+canary+`"},"b":{"type":"number","value":1}}`),
			wantErr: outputs.ErrMalformedValue,
		},
		{
			name: "list of objects of different types", out: output(`["list",["object",{"a":"dynamic"}]]`, `[{"a":{"type":"string","value":"`+canary+`"}},{"a":{"type":"number","value":1}}]`),
			wantErr: outputs.ErrMalformedValue,
		},
		{name: "dynamic without its type", out: output(`"dynamic"`, `"`+canary+`"`), wantErr: outputs.ErrMalformedValue},
		{name: "dynamic with an unknown type", out: output(`"dynamic"`, `{"type":"text","value":"`+canary+`"}`), wantErr: outputs.ErrMalformedValue},
		{name: "dynamic with an extra key", out: output(`"dynamic"`, `{"type":"string","value":"acme","`+canary+`":1}`), wantErr: outputs.ErrMalformedValue},
		{name: "dynamic without its value", out: output(`"dynamic"`, `{"type":"string"}`), wantErr: outputs.ErrMalformedValue},
		{name: "dynamic with a null type", out: output(`"dynamic"`, `{"type":null,"value":"`+canary+`"}`), wantErr: outputs.ErrMalformedValue},
		{name: "dynamic from a list", out: output(`"dynamic"`, `["`+canary+`"]`), wantErr: outputs.ErrMalformedValue},
		{
			name: "dynamic with extra data in its type", out: output(`"dynamic"`, `{"type":["list","string","number"],"value":["`+canary+`"]}`),
			wantErr: outputs.ErrMalformedValue,
		},
		{
			name: "dynamic with an optional attribute not in its type", out: output(`"dynamic"`, `{"type":["object",{"id":"string"},["port"]],"value":{"id":"`+canary+`"}}`),
			wantErr: outputs.ErrMalformedValue,
		},
		{
			name: "list of a resolved dynamic attribute and a null object", out: output(`["list",["object",{"a":"dynamic"}]]`, `[{"a":{"type":"string","value":"`+canary+`"}},null]`),
			wantErr: outputs.ErrMalformedValue,
		},
		{
			name: "set of dynamic elements of different types", out: output(`["set","dynamic"]`, `[{"type":"string","value":"`+canary+`"},{"type":"bool","value":true}]`),
			wantErr: outputs.ErrMalformedValue,
		},
		{name: "object from a list", out: output(`["object",{"id":"string"}]`, `["`+canary+`"]`), wantErr: outputs.ErrMalformedValue},
		{name: "map from a list", out: output(`["map","string"]`, `["`+canary+`"]`), wantErr: outputs.ErrMalformedValue},
		{name: "tuple from an object", out: output(`["tuple",["string"]]`, `{"id":"`+canary+`"}`), wantErr: outputs.ErrMalformedValue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := outputs.Value(tt.out)
			require.ErrorIs(t, err, tt.wantErr)
			assert.NotContains(t, err.Error(), canary)
			assert.Contains(t, err.Error(), tt.wantMsg)
			assert.True(t, got.IsNull())
		})
	}
}

func goldenOutputs(t *testing.T) map[string]v1.StackOutput {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "client", "testdata", "stack_outputs.json"))
	require.NoError(t, err)
	var body v1.StackOutputs
	require.NoError(t, json.Unmarshal(b, &body))
	return body.Outputs
}

func with[V any](m map[string]V, key string, value V) map[string]V {
	m = maps.Clone(m)
	m[key] = value
	return m
}

func TestObjectOfTheGoldenOutputs(t *testing.T) {
	endpoints := map[string]attr.Type{"dynamodb": types.StringType, "s3": types.StringType}
	attrTypes := map[string]attr.Type{
		"endpoints":          types.ObjectType{AttrTypes: endpoints},
		"nat_gateway_count":  types.NumberType,
		"private_subnet_ids": types.ListType{ElemType: types.StringType},
		"tags":               types.MapType{ElemType: types.StringType},
		"vpc_id":             types.StringType,
	}
	attrs := map[string]attr.Value{
		"endpoints": types.ObjectValueMust(endpoints, map[string]attr.Value{
			"dynamodb": str("vpce-0d4e5f6a7b8c9d0e1"),
			"s3":       str("vpce-0a1b2c3d4e5f6a7b8"),
		}),
		"nat_gateway_count":  num(3),
		"private_subnet_ids": types.ListValueMust(types.StringType, []attr.Value{str("subnet-0a1b2c3d4e5f6a7b8"), str("subnet-0b2c3d4e5f6a7b8c9")}),
		"tags": types.MapValueMust(types.StringType, map[string]attr.Value{
			"environment": str("prod"),
			"team":        str("platform"),
		}),
		"vpc_id": str("vpc-0a1b2c3d4e5f67890"),
	}
	secret := v1.StackOutput{Type: json.RawMessage(`"string"`), Value: json.RawMessage(`"` + canary + `"`), Sensitive: true}
	nonsensitive := func(_ string, out v1.StackOutput) bool { return !out.Sensitive }

	tests := []struct {
		name    string
		outputs map[string]v1.StackOutput
		include func(string, v1.StackOutput) bool
		want    types.Dynamic
	}{
		{name: "every output", outputs: goldenOutputs(t), want: types.DynamicValue(types.ObjectValueMust(attrTypes, attrs))},
		{
			name: "with a sensitive output", outputs: with(goldenOutputs(t), "db_password", secret),
			want: types.DynamicValue(types.ObjectValueMust(with(attrTypes, "db_password", attr.Type(types.StringType)), with(attrs, "db_password", attr.Value(str(canary))))),
		},
		{
			name: "nonsensitive with a sensitive output", outputs: with(goldenOutputs(t), "db_password", secret), include: nonsensitive,
			want: types.DynamicValue(types.ObjectValueMust(attrTypes, attrs)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := outputs.Object(tt.outputs, tt.include)
			require.NoError(t, err)
			assertSameValue(t, tt.want, got)
		})
	}
}

func TestObjectInclude(t *testing.T) {
	outs := map[string]v1.StackOutput{
		"db_password": {Type: json.RawMessage(`"string"`), Value: json.RawMessage(`"` + canary + `"`), Sensitive: true},
		"vpc_id":      output(`"string"`, `"vpc-0a1b2c3d4e5f67890"`),
		"zones":       output(`["list","string"]`, `["eu-west-1a","eu-west-1b"]`),
	}
	tests := []struct {
		name    string
		include func(string, v1.StackOutput) bool
		want    []string
	}{
		{name: "nil accepts every output", want: []string{"db_password", "vpc_id", "zones"}},
		{name: "nonsensitive", include: func(_ string, out v1.StackOutput) bool { return !out.Sensitive }, want: []string{"vpc_id", "zones"}},
		{name: "by name", include: func(name string, _ v1.StackOutput) bool { return name == "zones" }, want: []string{"zones"}},
		{name: "none", include: func(string, v1.StackOutput) bool { return false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := outputs.Object(outs, tt.include)
			require.NoError(t, err)
			require.False(t, got.IsNull())
			obj, ok := got.UnderlyingValue().(basetypes.ObjectValue)
			require.Truef(t, ok, "want an object, got %T", got.UnderlyingValue())
			assert.ElementsMatch(t, tt.want, slices.Collect(maps.Keys(obj.Attributes())))
			for _, name := range tt.want {
				want, err := outputs.Value(outs[name])
				require.NoError(t, err)
				assertSameValue(t, want, types.DynamicValue(obj.Attributes()[name]))
			}
		})
	}
}

func TestObjectIncludeSeesEachOutput(t *testing.T) {
	outs := goldenOutputs(t)
	seen := map[string]v1.StackOutput{}
	_, err := outputs.Object(outs, func(name string, out v1.StackOutput) bool {
		seen[name] = out
		return true
	})
	require.NoError(t, err)
	assert.Equal(t, outs, seen)
}

func TestObjectOfNoOutputs(t *testing.T) {
	tests := []struct {
		name    string
		outputs map[string]v1.StackOutput
		include func(string, v1.StackOutput) bool
	}{
		{name: "nil map"},
		{name: "empty map", outputs: map[string]v1.StackOutput{}},
		{name: "none accepted", outputs: goldenOutputs(t), include: func(string, v1.StackOutput) bool { return false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := outputs.Object(tt.outputs, tt.include)
			require.NoError(t, err)
			assert.False(t, got.IsNull())
			assert.False(t, got.IsUnknown())
			assertSameValue(t, types.DynamicValue(types.ObjectValueMust(map[string]attr.Type{}, map[string]attr.Value{})), got)

			tv, err := got.ToTerraformValue(context.Background())
			require.NoError(t, err)
			assert.True(t, tv.IsFullyKnown())
			assert.True(t, tv.Type().Equal(tftypes.Object{AttributeTypes: map[string]tftypes.Type{}}))
		})
	}
}

func TestObjectKeepsNullsTyped(t *testing.T) {
	got, err := outputs.Object(map[string]v1.StackOutput{
		"name":    output(`"string"`, `null`),
		"ports":   output(`["list","number"]`, `null`),
		"rule":    output(`["object",{"id":"string","port":"number"},["port"]]`, `null`),
		"unknown": output(`"dynamic"`, `null`),
	}, nil)
	require.NoError(t, err)
	assertSameValue(t, types.DynamicValue(types.ObjectValueMust(
		map[string]attr.Type{
			"name":    types.StringType,
			"ports":   types.ListType{ElemType: types.NumberType},
			"rule":    types.ObjectType{AttrTypes: portType},
			"unknown": types.DynamicType,
		},
		map[string]attr.Value{
			"name":    types.StringNull(),
			"ports":   types.ListNull(types.NumberType),
			"rule":    types.ObjectNull(portType),
			"unknown": types.DynamicNull(),
		},
	)), got)
}

func TestObjectNamesTheMalformedOutput(t *testing.T) {
	tests := []struct {
		name    string
		broken  v1.StackOutput
		include func(string, v1.StackOutput) bool
		wantErr error
	}{
		{name: "malformed type", broken: output(`"text"`, `"`+canary+`"`), wantErr: outputs.ErrMalformedType},
		{name: "malformed value", broken: output(`"number"`, `"`+canary+`"`), wantErr: outputs.ErrMalformedValue},
		{
			name: "malformed sensitive value", broken: v1.StackOutput{Type: json.RawMessage(`"bool"`), Value: json.RawMessage(`"` + canary + `"`), Sensitive: true},
			wantErr: outputs.ErrMalformedValue,
		},
		{
			name: "malformed output left out", broken: output(`"number"`, `"`+canary+`"`),
			include: func(name string, _ v1.StackOutput) bool { return name != "broken" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outs := map[string]v1.StackOutput{
				"broken": tt.broken,
				"vpc_id": output(`"string"`, `"vpc-0a1b2c3d4e5f67890"`),
			}
			got, err := outputs.Object(outs, tt.include)
			if tt.wantErr == nil {
				require.NoError(t, err)
				assertSameValue(t, types.DynamicValue(types.ObjectValueMust(
					map[string]attr.Type{"vpc_id": types.StringType},
					map[string]attr.Value{"vpc_id": str("vpc-0a1b2c3d4e5f67890")},
				)), got)
				return
			}
			require.ErrorIs(t, err, tt.wantErr)
			assert.Contains(t, err.Error(), `output "broken"`)
			assert.NotContains(t, err.Error(), canary)
			assert.True(t, got.IsNull())
		})
	}
}

func typeOfContents(t *testing.T, v tftypes.Value) tftypes.Type {
	t.Helper()
	if v.IsNull() {
		return v.Type()
	}
	switch typ := v.Type().(type) {
	case tftypes.Object:
		var attrs map[string]tftypes.Value
		require.NoError(t, v.As(&attrs))
		attrTypes := make(map[string]tftypes.Type, len(attrs))
		for name, a := range attrs {
			attrTypes[name] = typeOfContents(t, a)
		}
		return tftypes.Object{AttributeTypes: attrTypes}
	case tftypes.Tuple:
		var elems []tftypes.Value
		require.NoError(t, v.As(&elems))
		elemTypes := make([]tftypes.Type, len(elems))
		for i, e := range elems {
			elemTypes[i] = typeOfContents(t, e)
		}
		return tftypes.Tuple{ElementTypes: elemTypes}
	case tftypes.List, tftypes.Set:
		var elems []tftypes.Value
		require.NoError(t, v.As(&elems))
		if len(elems) == 0 {
			return typ
		}
		if _, ok := typ.(tftypes.Set); ok {
			return tftypes.Set{ElementType: typeOfContents(t, elems[0])}
		}
		return tftypes.List{ElementType: typeOfContents(t, elems[0])}
	case tftypes.Map:
		var elems map[string]tftypes.Value
		require.NoError(t, v.As(&elems))
		for _, e := range elems {
			return tftypes.Map{ElementType: typeOfContents(t, e)}
		}
		return typ
	}
	return v.Type()
}

func TestObjectTypeIsTheTypeOfItsAttributes(t *testing.T) {
	tests := []struct {
		name    string
		outputs map[string]v1.StackOutput
	}{
		{name: "golden", outputs: goldenOutputs(t)},
		{name: "none", outputs: map[string]v1.StackOutput{}},
		{name: "every kind", outputs: map[string]v1.StackOutput{
			"string":   output(`"string"`, `"acme"`),
			"number":   output(`"number"`, `0.1`),
			"large":    output(`"number"`, `123456789012345678901234567890.123456789`),
			"bool":     output(`"bool"`, `false`),
			"list":     output(`["list","string"]`, `["a","b"]`),
			"set":      output(`["set","number"]`, `[1,2]`),
			"map":      output(`["map","bool"]`, `{"a":true}`),
			"tuple":    output(`["tuple",["string","number"]]`, `["a",1]`),
			"object":   output(`["object",{"id":"string","port":"number"},["port"]]`, `{"id":"sg-0a1b"}`),
			"objects":  output(`["list",["object",{"id":"string","port":"number"},["port"]]]`, `[{"id":"sg-0a1b"}]`),
			"dynamic":  output(`"dynamic"`, `{"type":["tuple",["string","bool"]],"value":["a",true]}`),
			"attrs":    output(`["object",{"name":"dynamic","owner":"dynamic"}]`, `{"name":{"type":"string","value":"acme"},"owner":null}`),
			"elements": output(`["map","dynamic"]`, `{"a":{"type":["list","number"],"value":[1]}}`),
			"empty":    output(`["list","dynamic"]`, `[]`),
			"nulls":    output(`["list","dynamic"]`, `[null]`),
			"null":     output(`["object",{"id":"string"}]`, `null`),
			"nothing":  output(`"dynamic"`, `null`),
			"unified":  output(`["list","dynamic"]`, `[null,{"type":"string","value":"acme"}]`),
		}},
		{name: "optional attributes in dynamic values", outputs: map[string]v1.StackOutput{
			"null_object": output(`"dynamic"`, `{"type":["object",{"a":"string"},["a"]],"value":null}`),
			"empty_list":  output(`"dynamic"`, `{"type":["list",["object",{"a":"string"},["a"]]],"value":[]}`),
			"empty_map":   output(`"dynamic"`, `{"type":["map",["object",{"a":"string"},["a"]]],"value":{}}`),
			"null_list":   output(`"dynamic"`, `{"type":["list",["object",{"a":"string"},["a"]]],"value":null}`),
			"list":        output(`"dynamic"`, `{"type":["list",["object",{"a":"string"},["a"]]],"value":[{}]}`),
			"attribute":   output(`["object",{"x":"dynamic"}]`, `{"x":{"type":["object",{"a":"string"},["a"]],"value":null}}`),
		}},
	}
	schema := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"values": tftypes.DynamicPseudoType}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := outputs.Object(tt.outputs, nil)
			require.NoError(t, err)
			tv, err := got.ToTerraformValue(context.Background())
			require.NoError(t, err)
			require.True(t, tv.IsFullyKnown())

			obj, ok := tv.Type().(tftypes.Object)
			require.Truef(t, ok, "want an object type, got %s", tv.Type())
			var attrs map[string]tftypes.Value
			require.NoError(t, tv.As(&attrs))
			attrTypes := make(map[string]tftypes.Type, len(attrs))
			for name, a := range attrs {
				attrTypes[name] = a.Type()
			}
			assert.Truef(t, obj.Equal(tftypes.Object{AttributeTypes: attrTypes}), "object type %s", obj)
			assert.Truef(t, obj.Equal(typeOfContents(t, tv)), "object type %s, contents %s", obj, typeOfContents(t, tv))
			assert.Equal(t, slices.Sorted(maps.Keys(tt.outputs)), slices.Sorted(maps.Keys(attrs)))

			state := tftypes.NewValue(schema, map[string]tftypes.Value{"values": tv})
			wire, err := tfprotov6.NewDynamicValue(schema, state)
			require.NoError(t, err)
			back, err := wire.Unmarshal(schema)
			require.NoError(t, err)
			assert.Truef(t, state.Equal(back), "sent %s, received %s", state, back)
		})
	}
}
