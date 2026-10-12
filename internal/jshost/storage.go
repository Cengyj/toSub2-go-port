package jshost

import (
	_ "embed"
	"encoding/base64"

	"github.com/grafana/sobek"
)

//go:embed storage.js
var storageSource string

const storageQuotaBytes int64 = 5 << 20

// Each host owns its areas. Its event loop is the only reader/writer; different
// authentication hosts never share a map or a Sobek value.
type storageAreas struct {
	local   map[string]*storageArea
	session map[string]*storageArea
}

type storageEntry struct {
	value string
	units int64
}

type storageArea struct {
	values map[string]storageEntry
	keys   []string
	bytes  int64
}

func (areas *storageAreas) area(kind, origin string) *storageArea {
	if areas.local == nil {
		areas.local = make(map[string]*storageArea)
	}
	if areas.session == nil {
		areas.session = make(map[string]*storageArea)
	}
	selected := areas.local
	if kind == "session" {
		selected = areas.session
	}
	if selected[origin] == nil {
		selected[origin] = &storageArea{values: make(map[string]storageEntry)}
	}
	return selected[origin]
}

func (area *storageArea) set(key, value string, units int64) bool {
	prior, present := area.values[key]
	nextBytes := area.bytes + 2*(units-prior.units)
	if nextBytes > storageQuotaBytes {
		return false
	}
	if !present {
		area.keys = append(area.keys, key)
	}
	area.values[key] = storageEntry{value: value, units: units}
	area.bytes = nextBytes
	return true
}

func (area *storageArea) remove(key string) {
	entry, present := area.values[key]
	if !present {
		return
	}
	delete(area.values, key)
	area.bytes -= 2 * entry.units
	for index, existing := range area.keys {
		if existing == key {
			area.keys = append(area.keys[:index], area.keys[index+1:]...)
			break
		}
	}
}

// installStorage exposes only Storage values and methods. Keys/values cross
// the Go bridge as strings, encoding surrogate code units as UTF-16 buffers,
// so lone UTF-16 surrogates survive
// without being merged with U+FFFD by a Go string conversion.
func installStorage(vm *sobek.Runtime, backing *storageAreas, origin string) error {
	if backing == nil {
		return &Error{Code: "HOST_INITIALIZATION_FAILED"}
	}
	native := vm.NewObject()
	_ = native.Set("encode", func(call sobek.FunctionCall) sobek.Value {
		buffer, ok := call.Argument(0).Export().(sobek.ArrayBuffer)
		if !ok {
			panic(vm.NewTypeError("Storage encoding invalid"))
		}
		return vm.ToValue(base64.StdEncoding.EncodeToString(buffer.Bytes()))
	})
	_ = native.Set("decode", func(value string) sobek.Value {
		bytes, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			panic(vm.NewTypeError("Storage encoding invalid"))
		}
		return vm.ToValue(vm.NewArrayBuffer(bytes))
	})
	_ = native.Set("opaque", origin == "null" || origin == "")
	_ = native.Set("length", func(kind string) int { return len(backing.area(kind, origin).keys) })
	_ = native.Set("keys", func(kind string) []string {
		return append([]string{}, backing.area(kind, origin).keys...)
	})
	_ = native.Set("get", func(kind, key string) sobek.Value {
		if value, present := backing.area(kind, origin).values[key]; present {
			return vm.ToValue(value.value)
		}
		return sobek.Null()
	})
	_ = native.Set("set", func(kind, key, value string, units int64) bool {
		return backing.area(kind, origin).set(key, value, units)
	})
	_ = native.Set("remove", func(kind, key string) { backing.area(kind, origin).remove(key) })
	_ = native.Set("clear", func(kind string) {
		area := backing.area(kind, origin)
		area.values = make(map[string]storageEntry)
		area.keys = nil
		area.bytes = 0
	})
	if err := vm.Set("__storageHost", native); err != nil {
		return err
	}
	_, err := vm.RunScript("browser-storage.js", storageSource)
	return err
}
