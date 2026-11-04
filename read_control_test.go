package uio_test

import (
	"reflect"
	"testing"

	"github.com/urpc/uio"
	"github.com/urpc/uio/umux"
)

// Flow-control ownership is an internal composition mechanism. Public
// connection interfaces promise only the application's pause/resume switch
// and the cooperative consumer checkpoint.
func TestReadControlPublicSurface(t *testing.T) {
	want := map[string]bool{"PauseReads": true, "ResumeReads": true, "ReadsPaused": true}
	controller := reflect.TypeOf((*uio.ReadController)(nil)).Elem()
	if controller.NumMethod() != len(want) {
		t.Fatalf("ReadController has %d methods, want %d", controller.NumMethod(), len(want))
	}
	for i := 0; i < controller.NumMethod(); i++ {
		if method := controller.Method(i); !want[method.Name] {
			t.Errorf("internal capability exposed on ReadController: %s", method.Name)
		}
	}
	for _, kind := range []reflect.Type{
		reflect.TypeOf((*uio.Conn)(nil)).Elem(),
		reflect.TypeOf((*uio.LayerDown)(nil)).Elem(),
		reflect.TypeOf((*umux.Stream)(nil)),
	} {
		for _, name := range []string{"NewReadPause", "NewReadControl", "ReadControl", "InternalReadControl", "GetReadControl"} {
			if _, exists := kind.MethodByName(name); exists {
				t.Errorf("internal method %s exposed on %s", name, kind)
			}
		}
	}
	// Embedding an exported internal adapter directly would leave an exported
	// field on Stream even if all of the adapter's methods were hidden.
	if field, exists := reflect.TypeOf(umux.Stream{}).FieldByName("Access"); exists && field.IsExported() {
		t.Fatal("internal read-control adapter exposed as an exported Stream field")
	}
}
