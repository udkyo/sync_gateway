package clistruct

import (
	"errors"
	"fmt"
	"reflect"
)

// reflectStructValue returns the reflect.Value of the given val struct, also following pointers.
// Panics if the given val was nil or not a struct.
func reflectStructValue(val interface{}) (refVal reflect.Value, err error) {
	refVal = reflect.ValueOf(val)
	if refVal.IsZero() {
		return refVal, errors.New("can't get reflect.Value of nil")
	}

	derefVal := reflect.Indirect(refVal)
	if derefVal.Kind() != reflect.Struct {
		return refVal, fmt.Errorf("expected val to be a struct, but was %s", derefVal.Kind())
	}

	return derefVal, nil
}

// reflectInterfaceValue returns the underlying value as an untyped interface{},
// always returning a pointer value.
func reflectInterfaceValue(v reflect.Value) interface{} {
	if v.Type().Kind() == reflect.Interface {
		return v.Interface()
	} else {
		return v.Addr().Interface()
	}
}
