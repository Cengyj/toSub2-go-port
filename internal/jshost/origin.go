package jshost

import whatwg "github.com/nlnwa/whatwg-url/url"

// Serialization is sufficient for tuple origins, but every opaque document
// needs its own identity. A non-zero-sized token prevents Go from coalescing
// distinct allocations. Inherited blank documents retain the creator's token.
type opaqueOrigin struct{ marker byte }
type documentOrigin struct {
	serialized string
	opaque     *opaqueOrigin
}

func documentOriginForURL(raw string, parent *realm) documentOrigin {
	parsed, err := whatwg.Parse(raw)
	if err == nil && parent != nil && parsed.Scheme() == "about" && parsed.Pathname() == "blank" {
		return parent.documentOrigin
	}
	serialized := origin(raw)
	if serialized == "null" {
		return documentOrigin{serialized: "null", opaque: new(opaqueOrigin)}
	}
	return documentOrigin{serialized: serialized}
}

func (r *realm) sameOrigin(target *realm) bool {
	return r.documentOrigin == target.documentOrigin
}

func (r *realm) messageTargetMatches(target *realm, requested string) bool {
	switch requested {
	case "*":
		return true
	case "/":
		return r.sameOrigin(target)
	default:
		serialized := origin(requested)
		if serialized == "null" {
			panic(r.vm.NewTypeError("Invalid target origin"))
		}
		return serialized == target.documentOrigin.serialized
	}
}
