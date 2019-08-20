package handler

import (
	"reflect"
	"strings"
	"testing"
)

func TestReadProperties(t *testing.T) {
	properties, err := readProperties(strings.NewReader("foo=Hello\n bar = World \nbaz=!"))
	if err != nil {
		t.Error(err)
	}
	if !reflect.DeepEqual(properties, map[string]string{
		"foo": "Hello",
		"bar": "World",
		"baz": "!",
	}) {
		t.Error(properties)
	}
}

func TestExpandProperty(t *testing.T) {
	properties := map[string]string{
		"foo": "Hello {bar} {baz}",
		"bar": "{baz} World",
		"baz": "!",
	}
	foo := expandProperty(properties, "foo")
	if foo != "Hello ! World !" {
		t.Error(foo)
	}
}
