package jshost

import (
	"testing"

	"github.com/grafana/sobek"
)

func storageVM(t *testing.T, areas *storageAreas, origin string) *sobek.Runtime {
	t.Helper()
	vm := sobek.New()
	if _, err := vm.RunString(`class DOMException extends Error {constructor(message,name){super(message);this.name=name;}}`); err != nil {
		t.Fatal(err)
	}
	if err := installStorage(vm, areas, origin); err != nil {
		t.Fatal(err)
	}
	return vm
}

func storageEval(t *testing.T, vm *sobek.Runtime, source string) string {
	t.Helper()
	value, err := vm.RunString(source)
	if err != nil {
		t.Fatal(err)
	}
	return value.String()
}

func TestStorageHostOriginAndAreaIsolation(t *testing.T) {
	areas := &storageAreas{}
	one := storageVM(t, areas, "https://same.test")
	two := storageVM(t, areas, "https://same.test")
	otherOrigin := storageVM(t, areas, "https://other.test")
	otherHost := storageVM(t, &storageAreas{}, "https://same.test")
	storageEval(t, one, `localStorage.setItem('identity','local');sessionStorage.setItem('identity','session');`)
	if got := storageEval(t, two, `JSON.stringify([localStorage.identity,sessionStorage.identity])`); got != `["local","session"]` {
		t.Fatal("same-origin frame did not share host storage", got)
	}
	for _, vm := range []*sobek.Runtime{otherOrigin, otherHost} {
		if got := storageEval(t, vm, `JSON.stringify([localStorage.getItem('identity'),sessionStorage.getItem('identity')])`); got != `[null,null]` {
			t.Fatal("storage crossed origin or host isolation", got)
		}
	}
	storageEval(t, two, `localStorage.removeItem('identity');`)
	if got := storageEval(t, one, `JSON.stringify([localStorage.getItem('identity'),sessionStorage.identity])`); got != `[null,"session"]` {
		t.Fatal("shared removal changed another area", got)
	}
}

func TestStoragePropertiesCoercionAndPrototype(t *testing.T) {
	vm := storageVM(t, &storageAreas{}, "https://fixture.test")
	got := storageEval(t, vm, `
const s=localStorage;s.setItem('first',1);s.second=null;s.setItem('first',2);s.setItem('getItem','hidden');s.length='length-key';s.__proto__='proto-key';Object.defineProperty(s,'defined',{value:9});
const before={length:s.length,keys:Object.keys(s),names:Object.getOwnPropertyNames(s),key0:s.key(0),key1:s.key(1),missing:s.key(99),values:[s.first,s.second,s.defined,s.getItem('getItem'),s.getItem('length'),s.getItem('__proto__')],type:Object.prototype.toString.call(s),prototype:Object.getPrototypeOf(s)===Storage.prototype,instance:s instanceof Storage,has:'first'in s,descriptor:Object.getOwnPropertyDescriptor(s,'first')};
delete s.first;s.removeItem('second');s.clear();JSON.stringify({before,after:s.length});`)
	want := `{"before":{"length":6,"keys":["first","second","defined"],"names":["first","second","defined"],"key0":"first","key1":"second","missing":null,"values":["2","null","9","hidden","length-key","proto-key"],"type":"[object Storage]","prototype":true,"instance":true,"has":true,"descriptor":{"value":"2","writable":true,"enumerable":true,"configurable":true}},"after":0}`
	if got != want {
		t.Fatalf("storage semantics=%s", got)
	}
	if got := storageEval(t, vm, `(()=>{const results=[];for(const action of [()=>new Storage(),()=>Storage.prototype.getItem.call({},'key'),()=>localStorage.getItem(),()=>localStorage.setItem('key'),()=>localStorage.removeItem(),()=>localStorage.key(),()=>localStorage.getItem(Symbol()),()=>localStorage.setItem('symbol',Symbol()),()=>{localStorage.symbol=Symbol()}]){try{action();results.push(false)}catch(e){results.push(e instanceof TypeError)}}return JSON.stringify(results)})()`); got != `[true,true,true,true,true,true,true,true,true]` {
		t.Fatal("storage constructor/brand/argument validation failed", got)
	}
}

func TestStoragePreservesDOMStringCodeUnits(t *testing.T) {
	vm := storageVM(t, &storageAreas{}, "https://fixture.test")
	if got := storageEval(t, vm, `localStorage.setItem('\ud800','\udc00');localStorage.setItem('\ufffd','different');JSON.stringify({length:localStorage.length,key:localStorage.key(0).charCodeAt(0),value:localStorage.getItem('\ud800').charCodeAt(0),replacement:localStorage.getItem('\ufffd')})`); got != `{"length":2,"key":55296,"value":56320,"replacement":"different"}` {
		t.Fatal("DOMString surrogate or replacement character was lost", got)
	}
}

func TestStorageQuotaReplacementAndRecovery(t *testing.T) {
	vm := storageVM(t, &storageAreas{}, "https://fixture.test")
	got := storageEval(t, vm, `
const fill='x'.repeat(5*1024*1024/2-1);localStorage.setItem('k',fill);let rejected=false;try{localStorage.setItem('k',fill+'x')}catch(e){rejected=e.name==='QuotaExceededError'}
const retained=localStorage.getItem('k')===fill;localStorage.setItem('k','small');localStorage.setItem('next','ok');localStorage.removeItem('next');localStorage.clear();localStorage.setItem('k',fill);sessionStorage.setItem('k',fill);JSON.stringify({rejected,retained,local:localStorage.length,session:sessionStorage.length});`)
	if got != `{"rejected":true,"retained":true,"local":1,"session":1}` {
		t.Fatal("storage quota was not atomic or did not recover", got)
	}
}

func TestStorageOpaqueOriginsAreUnavailable(t *testing.T) {
	for _, origin := range []string{"", "null"} {
		vm := storageVM(t, &storageAreas{}, origin)
		if got := storageEval(t, vm, `(()=>{const errors=[];for(const name of ['localStorage','sessionStorage']){try{globalThis[name];errors.push('allowed')}catch(e){errors.push(e.name)}}return JSON.stringify(errors)})()`); got != `["SecurityError","SecurityError"]` {
			t.Fatal("opaque origin obtained storage", got)
		}
	}
}
