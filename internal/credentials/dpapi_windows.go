//go:build windows

package credentials

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

var dpapiEntropy = []byte("toSub2.credentials.v1")

func protectDPAPI(plain []byte) ([]byte, error) {
	return dpapiCall(plain, true)
}
func unprotectDPAPI(ciphertext []byte) ([]byte, error) {
	return dpapiCall(ciphertext, false)
}
func dpapiCall(input []byte, protect bool) ([]byte, error) {
	if len(input) == 0 {
		return nil, errors.New("empty DPAPI payload")
	}
	in := windows.DataBlob{Size: uint32(len(input)), Data: &input[0]}
	ent := windows.DataBlob{Size: uint32(len(dpapiEntropy)), Data: &dpapiEntropy[0]}
	var out windows.DataBlob
	var err error
	if protect {
		err = windows.CryptProtectData(&in, nil, &ent, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	} else {
		err = windows.CryptUnprotectData(&in, nil, &ent, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	}
	if err != nil {
		return nil, err
	}
	if out.Data == nil || out.Size == 0 {
		return nil, errors.New("DPAPI returned an empty result")
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	// CryptProtectData owns this allocation. Copy before LocalFree executes.
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
