package jshost

import (
	"strconv"

	"github.com/grafana/sobek"
)

// A membrane keeps realm-owned objects inside their engine. Even same-origin
// windows cannot pass Sobek Objects directly into another Runtime.
type foreignRef struct {
	realm  *realm
	object *sobek.Object
}

type foreignObject struct {
	local, remote *realm
	object        *sobek.Object
}

func (p *foreignObject) Get(key string) sobek.Value {
	return p.local.importValue(p.remote, p.object.Get(key))
}
func (p *foreignObject) Set(key string, value sobek.Value) bool {
	return p.object.Set(key, p.remote.importValue(p.local, value)) == nil
}
func (p *foreignObject) Delete(key string) bool { return p.object.Delete(key) == nil }
func (p *foreignObject) Has(key string) bool {
	fn, _ := sobek.AssertFunction(p.remote.vm.Get("Reflect").ToObject(p.remote.vm).Get("has"))
	v, err := fn(sobek.Undefined(), p.object, p.remote.vm.ToValue(key))
	return err == nil && v.ToBoolean()
}
func (p *foreignObject) Keys() []string { return p.object.Keys() }

func (r *realm) importValue(source *realm, value sobek.Value) sobek.Value {
	if value == nil || sobek.IsUndefined(value) {
		return sobek.Undefined()
	}
	if sobek.IsNull(value) {
		return sobek.Null()
	}
	if source == r {
		return value
	}
	object, ok := value.(*sobek.Object)
	if !ok {
		return r.vm.ToValue(value.Export())
	}
	if object == source.vm.GlobalObject() {
		return r.windowProxy(source)
	}
	if ref, ok := source.foreign[object]; ok {
		return r.importValue(ref.realm, ref.object)
	}
	for remote, proxy := range source.windows {
		if proxy == object {
			return r.windowProxy(remote.realm)
		}
	}
	ref := foreignRef{source, object}
	if existing := r.wrappers[ref]; existing != nil {
		return existing
	}
	forward := &foreignObject{r, source, object}
	target := r.vm.NewObject()
	traps := &sobek.ProxyTrapConfig{
		Get: func(_ *sobek.Object, key string, _ sobek.Value) sobek.Value { return forward.Get(key) },
		GetSym: func(_ *sobek.Object, key *sobek.Symbol, _ sobek.Value) sobek.Value {
			return r.importValue(source, object.GetSymbol(key))
		},
		Set: func(_ *sobek.Object, key string, value sobek.Value, _ sobek.Value) bool {
			return forward.Set(key, value)
		},
		Has:            func(_ *sobek.Object, key string) bool { return forward.Has(key) },
		DeleteProperty: func(_ *sobek.Object, key string) bool { return forward.Delete(key) },
		GetPrototypeOf: func(_ *sobek.Object) *sobek.Object {
			prototype := object.Prototype()
			if prototype == nil {
				return nil
			}
			return r.importValue(source, prototype).ToObject(r.vm)
		},
	}
	convertArgs := func(arguments []sobek.Value) []sobek.Value {
		out := make([]sobek.Value, len(arguments))
		for i, arg := range arguments {
			out[i] = source.importValue(r, arg)
		}
		return out
	}
	if function, ok := sobek.AssertFunction(value); ok {
		target = r.vm.ToValue(func(call sobek.ConstructorCall) *sobek.Object { return call.This }).ToObject(r.vm)
		traps.Apply = func(_ *sobek.Object, this sobek.Value, arguments []sobek.Value) sobek.Value {
			result, err := function(source.importValue(r, this), convertArgs(arguments)...)
			if err != nil {
				panic(r.vm.NewTypeError("Cross-realm function failed"))
			}
			return r.importValue(source, result)
		}
		traps.Construct = func(_ *sobek.Object, arguments []sobek.Value, _ *sobek.Object) *sobek.Object {
			result, err := source.vm.New(object, convertArgs(arguments)...)
			if err != nil {
				panic(r.vm.NewTypeError("Cross-realm constructor failed"))
			}
			return r.importValue(source, result).ToObject(r.vm)
		}
	} else {
		traps.OwnKeys = func(_ *sobek.Object) *sobek.Object { return r.vm.ToValue(object.GetOwnPropertyNames()).ToObject(r.vm) }
		traps.GetOwnPropertyDescriptor = func(_ *sobek.Object, key string) sobek.PropertyDescriptor {
			found := false
			for _, name := range object.GetOwnPropertyNames() {
				if name == key {
					found = true
					break
				}
			}
			if !found {
				return sobek.PropertyDescriptor{}
			}
			enumerable := sobek.FLAG_FALSE
			for _, name := range object.Keys() {
				if name == key {
					enumerable = sobek.FLAG_TRUE
					break
				}
			}
			return sobek.PropertyDescriptor{Value: forward.Get(key), Writable: sobek.FLAG_TRUE, Configurable: sobek.FLAG_TRUE, Enumerable: enumerable}
		}
	}
	wrapper := r.vm.ToValue(r.vm.NewProxy(target, traps)).ToObject(r.vm)
	r.wrappers[ref] = wrapper
	r.foreign[wrapper] = ref
	return wrapper
}

type windowRef struct{ realm *realm }
type windowObject struct {
	local  *realm
	target *windowRef
	post   sobek.Value
}

func (r *realm) windowProxy(target *realm) *sobek.Object {
	if target == r {
		return r.vm.GlobalObject()
	}
	if value := r.windows[target.window]; value != nil {
		return value
	}
	p := &windowObject{local: r, target: target.window}
	p.post = r.vm.ToValue(func(call sobek.FunctionCall) sobek.Value {
		target := p.target.realm
		if target.retired {
			return sobek.Undefined()
		}
		requested := call.Argument(1)
		transfer := call.Argument(2)
		if obj, ok := requested.(*sobek.Object); ok {
			requested = obj.Get("targetOrigin")
			transfer = obj.Get("transfer")
		}
		targetOrigin := "/"
		if requested != nil && !sobek.IsUndefined(requested) {
			targetOrigin = requested.String()
		}
		if targetOrigin == "/" {
			targetOrigin = origin(r.url)
		}
		if targetOrigin != "*" && origin(targetOrigin) == "null" {
			panic(r.vm.NewTypeError("Invalid target origin"))
		}
		copied := r.messageCopyTransfer(call.Argument(0), transfer)
		if targetOrigin != "*" {
			parsed := origin(targetOrigin)
			if parsed == "null" {
				panic(r.vm.NewTypeError("Invalid target origin"))
			}
			if parsed != origin(target.url) {
				return sobek.Undefined()
			}
		}
		if r.host.kind == "child" && target == r.host.root.parent {
			data := r.messagePlain(copied)
			if err := r.host.emit(map[string]any{"type": "parent-message", "message": map[string]any{"data": data, "graph": copied.graph, "targetOrigin": targetOrigin}}); err != nil {
				panic(r.vm.NewTypeError("Message bridge closed"))
			}
			return sobek.Undefined()
		}
		if r.host.kind == "parent" && target == r.host.activeFrame {
			data := r.messagePlain(copied)
			if err := r.host.emit(map[string]any{"type": "frame-message", "message": map[string]any{"data": data, "graph": copied.graph, "targetOrigin": targetOrigin}}); err != nil {
				panic(r.vm.NewTypeError("Message bridge closed"))
			}
			return sobek.Undefined()
		}
		if err := r.host.queueMessage(target, r, copied, r.url); err != nil {
			panic(r.vm.NewTypeError("Message bridge closed"))
		}
		return sobek.Undefined()
	})
	proxy := r.vm.NewDynamicObject(p)
	r.windows[target.window] = proxy
	return proxy
}

func (p *windowObject) sameOrigin() bool { return origin(p.local.url) == origin(p.target.realm.url) }
func (p *windowObject) denyAccess() {
	err, createErr := p.local.vm.New(p.local.vm.Get("DOMException"), p.local.vm.ToValue("SecurityError: cross-origin window access"), p.local.vm.ToValue("SecurityError"))
	if createErr != nil {
		panic(p.local.vm.NewTypeError("SecurityError"))
	}
	panic(err)
}
func (p *windowObject) Get(key string) sobek.Value {
	r, t := p.local, p.target.realm
	switch key {
	case "window", "self", "frames":
		return r.windowProxy(t)
	case "parent":
		if t.parent != nil {
			return r.windowProxy(t.parent)
		}
		return r.windowProxy(t)
	case "top":
		return r.windowProxy(t.top)
	case "postMessage":
		return p.post
	case "closed":
		return r.vm.ToValue(t.retired)
	case "length":
		return r.vm.ToValue(len(t.frames))
	case "then":
		return sobek.Undefined()
	}
	if index, err := strconv.Atoi(key); err == nil && index >= 0 {
		// DOM order, rather than Go map iteration, determines indexed frames.
		document := t.vm.Get("document").ToObject(t.vm)
		query, _ := sobek.AssertFunction(document.Get("querySelectorAll"))
		if query != nil {
			nodes, err := query(document, t.vm.ToValue("iframe"))
			if err == nil {
				node := nodes.ToObject(t.vm).Get(key)
				if node != nil && !sobek.IsUndefined(node) {
					child, err := t.frame(node.ToObject(t.vm))
					if err == nil {
						return r.windowProxy(child)
					}
				}
			}
		}
		return sobek.Undefined()
	}
	if !p.sameOrigin() {
		p.denyAccess()
	}
	return r.importValue(t, t.vm.Get(key))
}
func (p *windowObject) Set(key string, value sobek.Value) bool {
	if !p.sameOrigin() {
		p.denyAccess()
	}
	return p.target.realm.vm.Set(key, p.target.realm.importValue(p.local, value)) == nil
}
func (p *windowObject) Delete(key string) bool {
	if !p.sameOrigin() {
		p.denyAccess()
	}
	return p.target.realm.vm.GlobalObject().Delete(key) == nil
}
func (p *windowObject) Has(key string) bool {
	if !p.sameOrigin() {
		switch key {
		case "window", "self", "frames", "parent", "top", "postMessage", "closed", "length":
			return true
		}
		return false
	}
	return (&foreignObject{p.local, p.target.realm, p.target.realm.vm.GlobalObject()}).Has(key)
}
func (p *windowObject) Keys() []string {
	if !p.sameOrigin() {
		return []string{"window", "self", "frames", "parent", "top", "postMessage", "closed", "length"}
	}
	return p.target.realm.vm.GlobalObject().Keys()
}
