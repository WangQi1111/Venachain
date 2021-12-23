package abi

import (
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strings"
)

// indirect recursively dereferences the value until it either gets the value
// or finds a big.Int
func indirectV2(v reflect.Value) reflect.Value {
	if v.Kind() == reflect.Ptr && v.Elem().Type() != reflect.TypeOf(big.Int{}) {
		return indirect(v.Elem())
	}
	return v
}

// reflectIntType returns the reflect using the given size and
// unsignedness.
func reflectIntType(unsigned bool, size int) reflect.Type {
	if unsigned {
		switch size {
		case 8:
			return reflect.TypeOf(uint8(0))
		case 16:
			return reflect.TypeOf(uint16(0))
		case 32:
			return reflect.TypeOf(uint32(0))
		case 64:
			return reflect.TypeOf(uint64(0))
		}
	}
	switch size {
	case 8:
		return reflect.TypeOf(int8(0))
	case 16:
		return reflect.TypeOf(int16(0))
	case 32:
		return reflect.TypeOf(int32(0))
	case 64:
		return reflect.TypeOf(int64(0))
	}
	return reflect.TypeOf(&big.Int{})
}

// mapArgNamesToStructFields maps a slice of argument names to struct fields.
// first round: for each Exportable field that contains a `abi:""` tag
//   and this field name exists in the given argument name list, pair them together.
// second round: for each argument name that has not been already linked,
//   find what variable is expected to be mapped into, if it exists and has not been
//   used, pair them.
// Note this function assumes the given value is a struct value.
func mapArgNamesToStructFields(argNames []string, value reflect.Value) (map[string]string, error) {
	typ := value.Type()

	abi2struct := make(map[string]string)
	struct2abi := make(map[string]string)

	// first round ~~~
	for i := 0; i < typ.NumField(); i++ {
		structFieldName := typ.Field(i).Name

		// skip private struct fields.
		if structFieldName[:1] != strings.ToUpper(structFieldName[:1]) {
			continue
		}
		// skip fields that have no abi:"" tag.
		tagName, ok := typ.Field(i).Tag.Lookup("abi")
		if !ok {
			continue
		}
		// check if tag is empty.
		if tagName == "" {
			return nil, fmt.Errorf("struct: abi tag in '%s' is empty", structFieldName)
		}
		// check which argument field matches with the abi tag.
		found := false
		for _, arg := range argNames {
			if arg == tagName {
				if abi2struct[arg] != "" {
					return nil, fmt.Errorf("struct: abi tag in '%s' already mapped", structFieldName)
				}
				// pair them
				abi2struct[arg] = structFieldName
				struct2abi[structFieldName] = arg
				found = true
			}
		}
		// check if this tag has been mapped.
		if !found {
			return nil, fmt.Errorf("struct: abi tag '%s' defined but not found in abi", tagName)
		}
	}

	// second round ~~~
	for _, argName := range argNames {

		structFieldName := ToCamelCase(argName)

		if structFieldName == "" {
			return nil, fmt.Errorf("abi: purely underscored output cannot unpack to struct")
		}

		// this abi has already been paired, skip it... unless there exists another, yet unassigned
		// struct field with the same field name. If so, raise an error:
		//    abi: [ { "name": "value" } ]
		//    struct { Value  *big.Int , Value1 *big.Int `abi:"value"`}
		if abi2struct[argName] != "" {
			if abi2struct[argName] != structFieldName &&
				struct2abi[structFieldName] == "" &&
				value.FieldByName(structFieldName).IsValid() {
				return nil, fmt.Errorf("abi: multiple variables maps to the same abi field '%s'", argName)
			}
			continue
		}

		// return an error if this struct field has already been paired.
		if struct2abi[structFieldName] != "" {
			return nil, fmt.Errorf("abi: multiple outputs mapping to the same struct field '%s'", structFieldName)
		}

		if value.FieldByName(structFieldName).IsValid() {
			// pair them
			abi2struct[argName] = structFieldName
			struct2abi[structFieldName] = argName
		} else {
			// not paired, but annotate as used, to detect cases like
			//   abi : [ { "name": "value" }, { "name": "_value" } ]
			//   struct { Value *big.Int }
			struct2abi[structFieldName] = argName
		}
	}
	return abi2struct, nil
}

// set attempts to assign src to dst by either setting, copying or otherwise.
//
// set is a bit more lenient when it comes to assignment and doesn't force an as
// strict ruleset as bare `reflect` does.
func setV2(dst, src reflect.Value) error {
	dstType, srcType := dst.Type(), src.Type()
	switch {
	case dstType.Kind() == reflect.Interface && dst.Elem().IsValid():
		return setV2(dst.Elem(), src)
	case dstType.Kind() == reflect.Ptr && dstType.Elem() != reflect.TypeOf(big.Int{}):
		return setV2(dst.Elem(), src)
	case srcType.AssignableTo(dstType) && dst.CanSet():
		dst.Set(src)
	case dstType.Kind() == reflect.Slice && srcType.Kind() == reflect.Slice && dst.CanSet():
		return setSlice(dst, src)
	case dstType.Kind() == reflect.Array:
		return setArray(dst, src)
	case dstType.Kind() == reflect.Struct:
		return setStruct(dst, src)
	default:
		return fmt.Errorf("abi: cannot unmarshal %v in to %v", src.Type(), dst.Type())
	}
	return nil
}

// setSlice attempts to assign src to dst when slices are not assignable by default
// e.g. src: [][]byte -> dst: [][15]byte
// setSlice ignores if we cannot copy all of src' elements.
func setSlice(dst, src reflect.Value) error {
	slice := reflect.MakeSlice(dst.Type(), src.Len(), src.Len())
	for i := 0; i < src.Len(); i++ {
		if src.Index(i).Kind() == reflect.Struct {
			if err := setV2(slice.Index(i), src.Index(i)); err != nil {
				return err
			}
		} else {
			// e.g. [][32]uint8 to []common.Hash
			if err := setV2(slice.Index(i), src.Index(i)); err != nil {
				return err
			}
		}
	}
	if dst.CanSet() {
		dst.Set(slice)
		return nil
	}
	return errors.New("Cannot set slice, destination not settable")
}

func setArray(dst, src reflect.Value) error {
	array := reflect.New(dst.Type()).Elem()
	min := src.Len()
	if src.Len() > dst.Len() {
		min = dst.Len()
	}
	for i := 0; i < min; i++ {
		if err := setV2(array.Index(i), src.Index(i)); err != nil {
			return err
		}
	}
	if dst.CanSet() {
		dst.Set(array)
		return nil
	}
	return errors.New("Cannot set array, destination not settable")
}

func setStruct(dst, src reflect.Value) error {
	for i := 0; i < src.NumField(); i++ {
		srcField := src.Field(i)
		dstField := dst.Field(i)
		if !dstField.IsValid() || !srcField.IsValid() {
			return fmt.Errorf("Could not find src field: %v value: %v in destination", srcField.Type().Name(), srcField)
		}
		if err := setV2(dstField, srcField); err != nil {
			return err
		}
	}
	return nil
}