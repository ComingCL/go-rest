package rest

import (
	"net/url"
	"reflect"
	"testing"
)

func TestRequestURI(t *testing.T) {
	r := (&Request{}).Param("a", "b")
	r.Prefix("other")
	r.RequestURI("test?foo=b&a=b&b=c&c=2")
	if r.pathPrefix != "/test" {
		t.Errorf("path is wrong: %#v", r)
	}
	if !reflect.DeepEqual(r.params, url.Values{"a": []string{"b"}, "foo": []string{"b"}, "b": []string{"c"}, "c": []string{"2"}}) {
		t.Errorf("should have set a param: %#v", r)
	}
}
