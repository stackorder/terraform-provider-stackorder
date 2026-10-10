// Package outputs turns the outputs of a stack, as the Stackorder server
// serves them, into the dynamic values of the stackorder_outputs data source
// and ephemeral resource. Each output arrives as its value in JSON beside its
// type in the JSON syntax Terraform writes to state. The conversion keeps
// that type exactly, so a tuple stays a tuple and an object stays an object,
// reads numbers at the 512-bit precision Terraform itself uses, and keeps
// nulls typed. Errors say what is malformed and never quote a value, which
// may be sensitive.
package outputs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	v1 "github.com/stackorder/stackorder/api/v1"
)

var (
	// ErrMalformedType means an output's type is missing or is not a type in
	// Terraform's JSON type syntax.
	ErrMalformedType = errors.New("malformed type")
	// ErrMalformedValue means an output's value is missing, is not JSON or
	// does not conform to its type as Terraform reads JSON values, which also
	// takes a number or bool for a string, a string that spells a number or
	// bool for one, and a set that repeats an element.
	ErrMalformedValue = errors.New("malformed value")
)

// Value converts one output into a dynamic value whose underlying value has
// the output's type, less the optional attribute markers of its object
// types, which only type constraints carry. A null value converts to a null
// of that type, or to a null dynamic value when the type is "dynamic". A
// value of type "dynamic", at the top or nested, carries its own type as
// {"type": ..., "value": ...}, Terraform's JSON encoding of dynamically
// typed values, and converts to a value of that type. A list, set or map of
// dynamic elements takes the one type its elements have, with null elements
// as nulls of it, as Terraform builds such collections. On error Value
// returns a null dynamic value as well, so only the error tells a failure
// apart from a null output of type "dynamic".
func Value(out v1.StackOutput) (types.Dynamic, error) {
	v, err := terraformValue(out)
	if err != nil {
		return types.DynamicNull(), err
	}
	return dynamic(v)
}

// Object converts the outputs include accepts into one dynamic value that
// holds an object with an attribute per output, named after it and
// converted as Value converts it. A nil include accepts every output. When
// include accepts none the object is empty, never null. The first accepted
// output by name that does not convert fails the call with an error that
// names it.
func Object(outputs map[string]v1.StackOutput, include func(name string, out v1.StackOutput) bool) (types.Dynamic, error) {
	attrTypes := make(map[string]tftypes.Type, len(outputs))
	attrs := make(map[string]tftypes.Value, len(outputs))
	for _, name := range slices.Sorted(maps.Keys(outputs)) {
		out := outputs[name]
		if include != nil && !include(name, out) {
			continue
		}
		v, err := terraformValue(out)
		if err != nil {
			return types.DynamicNull(), fmt.Errorf("output %q: %w", name, err)
		}
		attrTypes[name] = v.Type()
		attrs[name] = v
	}
	v, err := newValue(tftypes.Object{AttributeTypes: attrTypes}, attrs)
	if err != nil {
		return types.DynamicNull(), fmt.Errorf("%w: the outputs do not form an object", ErrMalformedValue)
	}
	return dynamic(v)
}

func terraformValue(out v1.StackOutput) (tftypes.Value, error) {
	if len(out.Type) == 0 {
		return tftypes.Value{}, fmt.Errorf("%w: it is missing", ErrMalformedType)
	}
	typ, err := parseType(out.Type)
	if err != nil {
		return tftypes.Value{}, fmt.Errorf("%w: %w", ErrMalformedType, err)
	}
	switch {
	case len(out.Value) == 0:
		return tftypes.Value{}, fmt.Errorf("%w: it is missing", ErrMalformedValue)
	case !json.Valid(out.Value):
		return tftypes.Value{}, fmt.Errorf("%w: it is not JSON", ErrMalformedValue)
	}
	v, err := decode(out.Value, typ)
	if err != nil {
		return tftypes.Value{}, fmt.Errorf("%w: it does not conform to its type", ErrMalformedValue)
	}
	return v, nil
}

func parseType(raw json.RawMessage) (tftypes.Type, error) {
	var name string
	if json.Unmarshal(raw, &name) == nil {
		switch name {
		case "string":
			return tftypes.String, nil
		case "number":
			return tftypes.Number, nil
		case "bool":
			return tftypes.Bool, nil
		case "dynamic":
			return tftypes.DynamicPseudoType, nil
		}
		return nil, fmt.Errorf("unknown primitive type %q", name)
	}
	var parts []json.RawMessage
	var kind string
	if json.Unmarshal(raw, &parts) != nil || len(parts) < 2 || json.Unmarshal(parts[0], &kind) != nil {
		return nil, errors.New("it is neither a primitive type nor a kind with its parameters")
	}
	switch kind {
	case "list", "set", "map":
		if len(parts) != 2 {
			return nil, fmt.Errorf("a %s type takes one element type", kind)
		}
		elem, err := parseType(parts[1])
		if err != nil {
			return nil, err
		}
		switch kind {
		case "list":
			return tftypes.List{ElementType: elem}, nil
		case "set":
			return tftypes.Set{ElementType: elem}, nil
		}
		return tftypes.Map{ElementType: elem}, nil
	case "tuple":
		var elems []json.RawMessage
		if len(parts) != 2 || json.Unmarshal(parts[1], &elems) != nil || elems == nil {
			return nil, errors.New("a tuple type takes an array of element types")
		}
		elemTypes := make([]tftypes.Type, len(elems))
		for i, elem := range elems {
			t, err := parseType(elem)
			if err != nil {
				return nil, err
			}
			elemTypes[i] = t
		}
		return tftypes.Tuple{ElementTypes: elemTypes}, nil
	case "object":
		var attrs map[string]json.RawMessage
		if len(parts) > 3 || json.Unmarshal(parts[1], &attrs) != nil || attrs == nil {
			return nil, errors.New("an object type takes an object of attribute types and an optional array of optional attributes")
		}
		attrTypes := make(map[string]tftypes.Type, len(attrs))
		for name, attr := range attrs {
			t, err := parseType(attr)
			if err != nil {
				return nil, err
			}
			attrTypes[name] = t
		}
		if len(parts) == 3 {
			var optional []string
			if json.Unmarshal(parts[2], &optional) != nil || optional == nil {
				return nil, errors.New("the optional attributes of an object type are not an array of names")
			}
			for _, name := range optional {
				if _, ok := attrs[name]; !ok {
					return nil, fmt.Errorf("optional attribute %q is not an attribute of its object type", name)
				}
			}
		}
		return tftypes.Object{AttributeTypes: attrTypes}, nil
	}
	return nil, fmt.Errorf("unknown type kind %q", kind)
}

func decode(raw json.RawMessage, typ tftypes.Type) (tftypes.Value, error) {
	if string(bytes.TrimSpace(raw)) == "null" {
		return tftypes.NewValue(typ, nil), nil
	}
	switch typ := typ.(type) {
	case tftypes.List:
		elems, elemType, err := decodeElements(raw, typ.ElementType)
		if err != nil {
			return tftypes.Value{}, err
		}
		return newValue(tftypes.List{ElementType: elemType}, elems)
	case tftypes.Set:
		elems, elemType, err := decodeElements(raw, typ.ElementType)
		if err != nil {
			return tftypes.Value{}, err
		}
		return newValue(tftypes.Set{ElementType: elemType}, elems)
	case tftypes.Map:
		var raws map[string]json.RawMessage
		if err := json.Unmarshal(raw, &raws); err != nil {
			return tftypes.Value{}, err
		}
		elems := make(map[string]tftypes.Value, len(raws))
		for key, r := range raws {
			v, err := decode(r, typ.ElementType)
			if err != nil {
				return tftypes.Value{}, err
			}
			elems[key] = v
		}
		elemType := elementType(typ.ElementType, maps.Values(elems))
		for key, v := range elems {
			elems[key] = typedNull(v, elemType)
		}
		return newValue(tftypes.Map{ElementType: elemType}, elems)
	case tftypes.Tuple:
		var raws []json.RawMessage
		if err := json.Unmarshal(raw, &raws); err != nil {
			return tftypes.Value{}, err
		}
		if len(raws) != len(typ.ElementTypes) {
			return tftypes.Value{}, errors.New("the tuple has another number of elements than its type")
		}
		elemTypes := make([]tftypes.Type, len(raws))
		elems := make([]tftypes.Value, len(raws))
		for i, r := range raws {
			v, err := decode(r, typ.ElementTypes[i])
			if err != nil {
				return tftypes.Value{}, err
			}
			elemTypes[i], elems[i] = v.Type(), v
		}
		return newValue(tftypes.Tuple{ElementTypes: elemTypes}, elems)
	case tftypes.Object:
		var raws map[string]json.RawMessage
		if err := json.Unmarshal(raw, &raws); err != nil {
			return tftypes.Value{}, err
		}
		for name := range raws {
			if _, ok := typ.AttributeTypes[name]; !ok {
				return tftypes.Value{}, errors.New("the object has an attribute its type lacks")
			}
		}
		attrTypes := make(map[string]tftypes.Type, len(typ.AttributeTypes))
		attrs := make(map[string]tftypes.Value, len(typ.AttributeTypes))
		for name, attrType := range typ.AttributeTypes {
			v := tftypes.NewValue(attrType, nil)
			if r, ok := raws[name]; ok {
				var err error
				if v, err = decode(r, attrType); err != nil {
					return tftypes.Value{}, err
				}
			}
			attrTypes[name], attrs[name] = v.Type(), v
		}
		return newValue(tftypes.Object{AttributeTypes: attrTypes}, attrs)
	}
	if typ.Equal(tftypes.DynamicPseudoType) {
		return decodeDynamic(raw)
	}
	return tftypes.ValueFromJSONWithOpts(raw, typ, tftypes.ValueFromJSONOpts{})
}

func decodeDynamic(raw json.RawMessage) (tftypes.Value, error) {
	var wrapped map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return tftypes.Value{}, err
	}
	rawType, rawValue := wrapped["type"], wrapped["value"]
	if len(wrapped) != 2 || rawType == nil || rawValue == nil {
		return tftypes.Value{}, errors.New("a dynamic value is not an object of its type and value")
	}
	typ, err := parseType(rawType)
	if err != nil {
		return tftypes.Value{}, err
	}
	return decode(rawValue, typ)
}

func decodeElements(raw json.RawMessage, declared tftypes.Type) ([]tftypes.Value, tftypes.Type, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(raw, &raws); err != nil {
		return nil, nil, err
	}
	elems := make([]tftypes.Value, len(raws))
	for i, r := range raws {
		v, err := decode(r, declared)
		if err != nil {
			return nil, nil, err
		}
		elems[i] = v
	}
	elemType := elementType(declared, slices.Values(elems))
	for i, v := range elems {
		elems[i] = typedNull(v, elemType)
	}
	return elems, elemType, nil
}

func elementType(declared tftypes.Type, elems iter.Seq[tftypes.Value]) tftypes.Type {
	for v := range elems {
		if !v.Type().Equal(tftypes.DynamicPseudoType) {
			return v.Type()
		}
	}
	return declared
}

func typedNull(v tftypes.Value, typ tftypes.Type) tftypes.Value {
	if v.Type().Equal(tftypes.DynamicPseudoType) {
		return tftypes.NewValue(typ, nil)
	}
	return v
}

func newValue(typ tftypes.Type, val any) (v tftypes.Value, err error) {
	defer func() {
		// tftypes panics rather than failing on some invalid types, such as objects with optional attributes.
		if recover() != nil {
			v, err = tftypes.Value{}, errors.New("tftypes cannot build the value")
		}
	}()
	if err = tftypes.ValidateValue(typ, val); err != nil {
		return tftypes.Value{}, err
	}
	return tftypes.NewValue(typ, val), nil
}

func dynamic(v tftypes.Value) (types.Dynamic, error) {
	fv, err := basetypes.DynamicType{}.ValueFromTerraform(context.Background(), v)
	if err != nil {
		return types.DynamicNull(), fmt.Errorf("%w: the provider framework cannot hold it", ErrMalformedValue)
	}
	d, ok := fv.(types.Dynamic)
	if !ok {
		return types.DynamicNull(), fmt.Errorf("%w: the provider framework made a %T of it", ErrMalformedValue, fv)
	}
	return d, nil
}
