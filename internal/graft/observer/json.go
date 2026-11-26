package observer

import (
	"encoding/json"
	"reflect"
)

// emptyLists makes every list in a document a list.
//
// Go answers a slice that was never filled with `null`, and a client that
// walks `runs.length` on it falls over: the page goes blank instead of
// showing that there is no run yet. The contract of this API says "a list of
// runs", "a list of targets", "a list of events", so the answer must be `[]`.
//
// The walk happens at the edge, once, for every endpoint: a field added
// tomorrow is covered by the same rule as the fields of today.
func emptyLists(payload any) any {
	value := reflect.ValueOf(payload)
	if !value.IsValid() {
		return payload
	}
	// A copy that is addressable, so the fields of a struct can be filled.
	copied := reflect.New(value.Type()).Elem()
	copied.Set(value)
	fillLists(copied)
	return copied.Interface()
}

// fillLists walks a document and replaces a list that is absent with an
// empty one.
func fillLists(value reflect.Value) {
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !value.IsNil() {
			fillLists(value.Elem())
		}
	case reflect.Slice:
		if value.IsNil() {
			if value.CanSet() {
				value.Set(reflect.MakeSlice(value.Type(), 0, 0))
			}
			return
		}
		for i := 0; i < value.Len(); i++ {
			fillLists(value.Index(i))
		}
	case reflect.Array:
		for i := 0; i < value.Len(); i++ {
			fillLists(value.Index(i))
		}
	case reflect.Map:
		if value.IsNil() {
			if value.CanSet() {
				value.Set(reflect.MakeMap(value.Type()))
			}
			return
		}
		for _, key := range value.MapKeys() {
			item := value.MapIndex(key)
			// The value of a map is a copy; a list inside it is filled in
			// the copy and written back.
			copied := reflect.New(item.Type()).Elem()
			copied.Set(item)
			fillLists(copied)
			value.SetMapIndex(key, copied)
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			field := value.Field(i)
			if field.CanSet() {
				fillLists(field)
			}
		}
	}
}

// marshalJSON writes a document as JSON, with every list a list.
func marshalJSON(payload any) ([]byte, error) {
	return json.Marshal(emptyLists(payload))
}
