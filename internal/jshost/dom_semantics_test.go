package jshost

import (
	"net/url"
	"testing"

	"github.com/grafana/sobek"
)

// These fixtures exercise DOM semantics independently of network and frame
// loading. Their host records insertions; it never executes a remote script.
type domFixture struct {
	vm           *sobek.Runtime
	inserted     []*sobek.Object
	removed      []*sobek.Object
	cookie       string
	cookieWrites []string
}

func newDOMFixture(t *testing.T) *domFixture {
	t.Helper()
	f := &domFixture{vm: sobek.New(), cookie: "visible=initial"}
	vm := f.vm
	if err := vm.Set("window", vm.GlobalObject()); err != nil {
		t.Fatal(err)
	}
	if err := vm.Set("self", vm.GlobalObject()); err != nil {
		t.Fatal(err)
	}
	if err := vm.Set("__config", map[string]any{"url": "https://fixture.test/base/page", "html": "<!doctype html><html><head></head><body></body></html>"}); err != nil {
		t.Fatal(err)
	}
	bridge := vm.NewObject()
	_ = bridge.Set("parseHTML", func(call sobek.FunctionCall) sobek.Value {
		contextTag := ""
		if value := call.Argument(1); !sobek.IsUndefined(value) {
			contextTag = value.String()
		}
		return vm.ToValue(parseHTML(call.Argument(0).String(), contextTag))
	})
	_ = bridge.Set("resolveURL", func(value, base string) string {
		parsedBase, err := url.Parse(base)
		if err != nil {
			return value
		}
		parsedValue, err := url.Parse(value)
		if err != nil {
			return value
		}
		return parsedBase.ResolveReference(parsedValue).String()
	})
	_ = bridge.Set("inserted", func(call sobek.FunctionCall) sobek.Value {
		f.inserted = append(f.inserted, call.Argument(0).ToObject(vm))
		return sobek.Undefined()
	})
	_ = bridge.Set("removed", func(call sobek.FunctionCall) sobek.Value {
		f.removed = append(f.removed, call.Argument(0).ToObject(vm))
		return sobek.Undefined()
	})
	_ = bridge.Set("cookieGet", func() string { return f.cookie })
	_ = bridge.Set("cookieSet", func(value string) {
		f.cookieWrites = append(f.cookieWrites, value)
		f.cookie = value
	})
	_ = bridge.Set("postMessage", func() {})
	if err := vm.Set("__host", bridge); err != nil {
		t.Fatal(err)
	}
	f.eval(t, `globalThis.queueMicrotask=fn=>Promise.resolve().then(fn);globalThis.location={href:__config.url};`)
	if _, err := vm.RunScript("dom.js", domSource); err != nil {
		t.Fatal(err)
	}
	f.inserted = nil
	return f
}

func (f *domFixture) eval(t *testing.T, source string) sobek.Value {
	t.Helper()
	value, err := f.vm.RunString(source)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func (f *domFixture) expect(t *testing.T, source, want string) {
	t.Helper()
	if got := f.eval(t, source).String(); got != want {
		t.Fatalf("DOM fixture returned %q, want %q", got, want)
	}
}

func TestDOMEventPropagationAndTrust(t *testing.T) {
	f := newDOMFixture(t)
	f.eval(t, `
var parentNode=document.createElement('div'),target=document.createElement('span');
document.body.appendChild(parentNode);parentNode.appendChild(target);
var order=[],paths=[],trust=[];
for(const [node,name] of [[window,'window'],[document,'document'],[parentNode,'parent'],[target,'target']]){
  node.addEventListener('fixture',e=>{order.push(name+':capture:'+e.eventPhase);if(node===target)paths.push(e.composedPath()[0]===target&&e.composedPath().at(-1)===window)},{capture:true});
  node.addEventListener('fixture',e=>order.push(name+':bubble:'+e.eventPhase));
}
var event=new Event('fixture',{bubbles:true,cancelable:true});
target.addEventListener('fixture',e=>{trust.push(e.isTrusted);e.preventDefault()},{once:true});
var dispatchResult=__domDispatchTrusted(target,event);
var after={target:event.target===target,current:event.currentTarget,phase:event.eventPhase,path:event.composedPath().length,canceled:event.defaultPrevented,trusted:event.isTrusted};
target.dispatchEvent(event);trust.push(event.isTrusted);
var overrideRejected=false;try{Object.defineProperty(event,'isTrusted',{value:true})}catch(e){overrideRejected=e instanceof TypeError}
`)
	f.expect(t, `order.slice(0,8).join(',')`, "window:capture:1,document:capture:1,parent:capture:1,target:capture:2,target:bubble:2,parent:bubble:3,document:bubble:3,window:bubble:3")
	f.expect(t, `JSON.stringify({dispatchResult,after,paths,trust,overrideRejected})`, `{"dispatchResult":false,"after":{"target":true,"current":null,"phase":0,"path":0,"canceled":true,"trusted":true},"paths":[true,true],"trust":[true,false],"overrideRejected":true}`)
	f.eval(t, `var message;addEventListener('message',e=>message={data:e.data,origin:e.origin,source:e.source===parentNode,trusted:e.isTrusted,target:e.target===window});__dispatchMessage({value:7},parentNode,'https://source.test');`)
	f.expect(t, `JSON.stringify(message)`, `{"data":{"value":7},"origin":"https://source.test","source":true,"trusted":true,"target":true}`)
}

func TestDOMEventListenerChangesAndCancellation(t *testing.T) {
	f := newDOMFixture(t)
	f.eval(t, `
var target=new EventTarget(),calls=[];
var removed=()=>calls.push('removed'),late=()=>calls.push('late');
target.addEventListener('change',()=>{calls.push('first');target.removeEventListener('change',removed);target.addEventListener('change',late)},{once:true});
target.addEventListener('change',removed);target.dispatchEvent(new Event('change'));target.dispatchEvent(new Event('change'));
var passive=new Event('passive',{cancelable:true});target.addEventListener('passive',e=>e.preventDefault(),{passive:true});var passiveResult=target.dispatchEvent(passive);
var stops=[];target.addEventListener('stop',e=>{stops.push('one');e.stopImmediatePropagation()});target.addEventListener('stop',()=>stops.push('two'));target.onstop=()=>stops.push('handler');target.dispatchEvent(new Event('stop'));
var handlerEvent=new Event('handler',{cancelable:true});target.onhandler=()=>false;var handlerResult=target.dispatchEvent(handlerEvent);
`)
	f.expect(t, `JSON.stringify({calls,passiveResult,passivePrevented:passive.defaultPrevented,stops,handlerResult})`, `{"calls":["first","late"],"passiveResult":true,"passivePrevented":false,"stops":["one"],"handlerResult":false}`)
}

func TestDOMLiveTreeFragmentsAndOwnership(t *testing.T) {
	f := newDOMFixture(t)
	f.eval(t, `
var container=document.createElement('div'),children=container.children,nodes=container.childNodes;
var fragment=document.createDocumentFragment(),a=document.createElement('span'),b=document.createElement('span');a.id='a';b.id='b';
fragment.append(a,'text',b);container.appendChild(fragment);document.body.appendChild(container);
var initial={children:children.length,nodes:nodes.length,emptyFragment:fragment.childNodes.length,first:children[0]===a,named:children.namedItem('b')===b,connected:a.isConnected};
container.insertBefore(b,a);a.remove();
var moved={children:children.length,nodes:nodes.length,first:children[0]===b,detached:!a.isConnected,parent:a.parentNode};
var other=new HTMLDocument('https://other.test/'),clone=other.importNode(container,true);other.appendChild(clone);
var ownership=clone.ownerDocument===other&&clone.firstChild.ownerDocument===other&&container.ownerDocument===document;
var deep=document.cloneNode(true),clonedOwners=deep.documentElement.ownerDocument===deep&&deep.body.ownerDocument===deep;
var attr=document.createAttribute('data-fixture');attr.value='value';container.setAttributeNode(attr);
var attrOwner=attr.ownerDocument===document&&attr.ownerElement===container;
`)
	f.expect(t, `JSON.stringify({initial,moved,ownership,clonedOwners,attrOwner})`, `{"initial":{"children":2,"nodes":3,"emptyFragment":0,"first":true,"named":true,"connected":true},"moved":{"children":1,"nodes":2,"first":true,"detached":true,"parent":null},"ownership":true,"clonedOwners":true,"attrOwner":true}`)
}

func TestDOMDocumentInsertionValidationIsAtomic(t *testing.T) {
	f := newDOMFixture(t)
	f.eval(t, `
var isolated=new HTMLDocument('https://fixture.test/'),oldRoot=isolated.createElement('html');isolated.appendChild(oldRoot);
var newRoot=isolated.createElement('html'),returned=isolated.replaceChild(newRoot,oldRoot);
var replaced=returned===oldRoot&&isolated.documentElement===newRoot&&oldRoot.parentNode===null;
var bad=isolated.createDocumentFragment();bad.append(isolated.createElement('div'),isolated.createElement('span'));var rejected=false;
try{isolated.replaceChild(bad,newRoot)}catch(e){rejected=e.name==='HierarchyRequestError'}
var unchanged=isolated.documentElement===newRoot&&bad.childNodes.length===2;
var hierarchy=[];for(const node of [isolated.createTextNode('invalid'),isolated.createAttribute('invalid'),new DocumentType('html',isolated)]){try{isolated.appendChild(node);hierarchy.push(false)}catch(e){hierarchy.push(e.name==='HierarchyRequestError')}}
`)
	f.expect(t, `JSON.stringify({replaced,rejected,unchanged,hierarchy})`, `{"replaced":true,"rejected":true,"unchanged":true,"hierarchy":[true,true,true]}`)
}

func TestDOMHTMLParsingAndSelectors(t *testing.T) {
	f := newDOMFixture(t)
	f.eval(t, `
document.body.innerHTML='<section id="scope"><ul><li id="a:b" class="one" data-value="Ab-c"></li><li class="two"></li><li class="one"></li></ul><script>one</script><script src="/remote.js"></script></section>';
var scope=document.getElementById('scope'),list=scope.querySelector('ul'),snapshot=list.querySelectorAll('li');
var selected={groups:scope.querySelectorAll('li.two, script:not([src])').length,relation:scope.querySelectorAll(':scope > ul > li:nth-child(2n + 1)').length,sibling:scope.querySelector('li.one + li').className,general:scope.querySelectorAll('li.two ~ li.one').length,escaped:scope.querySelector('#a\\:b').id,attribute:scope.querySelector('[data-value|="ab" i]').id};
list.appendChild(document.createElement('li'));var staticList=snapshot.length===3&&list.children.length===4;
var table=document.createElement('table');table.insertAdjacentHTML('beforeend','<tr><td>first</td><td>second</td></tr>');
var tableContext=table.querySelectorAll(':scope > tbody > tr > td').length;
var first=list.firstElementChild;first.insertAdjacentHTML('afterend','<li id="after-one"></li><li id="after-two"></li>');
var adjacent=first.nextElementSibling.id+','+first.nextElementSibling.nextElementSibling.id;
var invalid=[];for(const selector of ['li:unsupported','div >','[']){try{scope.querySelector(selector);invalid.push(false)}catch(e){invalid.push(e.name==='SyntaxError')}}
`)
	f.expect(t, `JSON.stringify({selected,staticList,tableContext,adjacent,invalid})`, `{"selected":{"groups":2,"relation":2,"sibling":"two","general":1,"escaped":"a:b","attribute":"a:b"},"staticList":true,"tableContext":2,"adjacent":"after-one,after-two","invalid":[true,true,true]}`)
}

func TestDOMAttributesTextAndPrototypeIdentity(t *testing.T) {
	f := newDOMFixture(t)
	f.eval(t, `
var element=document.createElement('div');element.dataset.testValue='<&';element.classList.add('a','b','a');element.classList.remove('b');element.classList.toggle('c',true);element.style.display='none';element.style.setProperty('color','red','important');
element.innerHTML='Hello <span>world</span><!--hidden-->';element.normalize();var values={dataset:element.getAttribute('data-test-value'),keys:Object.keys(element.dataset),classes:[...element.classList],text:element.textContent,display:element.style.display,priority:element.style.getPropertyPriority('color'),html:element.innerHTML};
var types=[Object.prototype.toString.call(document),Object.prototype.toString.call(element),Object.prototype.toString.call(element.children),Object.prototype.toString.call(new MessageEvent('message'))];
var prototype=element instanceof HTMLDivElement&&element instanceof HTMLElement&&element instanceof Element&&element instanceof Node&&element instanceof EventTarget;
element.textContent='literal <&';var escaped=element.innerHTML;
`)
	f.expect(t, `JSON.stringify({values,types,prototype,escaped})`, `{"values":{"dataset":"<&","keys":["testValue"],"classes":["a","c"],"text":"Hello world","display":"none","priority":"important","html":"Hello <span>world</span><!--hidden-->"},"types":["[object HTMLDocument]","[object HTMLDivElement]","[object HTMLCollection]","[object MessageEvent]"],"prototype":true,"escaped":"literal &lt;&amp;"}`)
	f.eval(t, `var normalized=document.createElement('div');normalized.append('a','','b','');normalized.normalize();`)
	f.expect(t, `JSON.stringify({text:normalized.textContent,count:normalized.childNodes.length})`, `{"text":"ab","count":1}`)
}

func TestDOMMutationDetachedSubtreeUntilDelivery(t *testing.T) {
	f := newDOMFixture(t)
	f.eval(t, `
var parentNode=document.createElement('div'),child=document.createElement('span');parentNode.appendChild(child);document.body.appendChild(parentNode);
var records=[];var observer=new MutationObserver(values=>records.push(...values.map(r=>r.type+':'+(r.attributeName||''))));
observer.observe(parentNode,{subtree:true,attributes:true,childList:true});child.remove();child.setAttribute('before-delivery','yes');
`)
	f.expect(t, `JSON.stringify(records)`, `["childList:","attributes:before-delivery"]`)
	f.eval(t, `child.setAttribute('after-delivery','no');`)
	f.expect(t, `JSON.stringify(records)`, `["childList:","attributes:before-delivery"]`)
	f.eval(t, `parentNode.appendChild(child);observer.disconnect();observer.observe(parentNode,{subtree:true,attributes:true});child.remove();child.setAttribute('attributes-only','yes');`)
	f.expect(t, `JSON.stringify(records)`, `["childList:","attributes:before-delivery","attributes:attributes-only"]`)
}

func TestDOMMutationRecordsAndMicrotaskOrder(t *testing.T) {
	f := newDOMFixture(t)
	f.eval(t, `
var parentNode=document.createElement('div'),child=document.createElement('span');parentNode.appendChild(child);document.body.appendChild(parentNode);child.setAttribute('data-test','initial');
var records=[],order=['sync'];var observer=new MutationObserver(values=>{order.push('mutation');records.push(...values.map(r=>({type:r.type,name:r.attributeName,old:r.oldValue,added:r.addedNodes.length,removed:r.removedNodes.length,target:r.target===child?'child':'parent'})))});
observer.observe(parentNode,{subtree:true,attributes:true,attributeFilter:['data-test'],childList:true});observer.observe(child,{attributes:true,attributeOldValue:true,attributeFilter:['data-test']});
child.setAttribute('ignored','none');child.setAttribute('data-test','next');child.setAttribute('data-test','last');var added=document.createElement('b');parentNode.appendChild(added);parentNode.removeChild(added);
queueMicrotask(()=>order.push('user'));Promise.resolve().then(()=>order.push('promise'));
`)
	f.expect(t, `JSON.stringify({records,order})`, `{"records":[{"type":"attributes","name":"data-test","old":"initial","added":0,"removed":0,"target":"child"},{"type":"attributes","name":"data-test","old":"next","added":0,"removed":0,"target":"child"},{"type":"childList","name":null,"old":null,"added":1,"removed":0,"target":"parent"},{"type":"childList","name":null,"old":null,"added":0,"removed":1,"target":"parent"}],"order":["sync","mutation","user","promise"]}`)
	f.eval(t, `child.setAttribute('data-test','taken');var taken=observer.takeRecords();observer.disconnect();child.setAttribute('data-test','disconnected');`)
	f.expect(t, `JSON.stringify({taken:taken.map(r=>r.oldValue),delivered:records.length,queued:observer.takeRecords().length})`, `{"taken":["last"],"delivered":4,"queued":0}`)
}

func TestDOMInsertedResourcesResolveCurrentBase(t *testing.T) {
	f := newDOMFixture(t)
	f.eval(t, `var holder=document.createElement('div'),script=document.createElement('script');script.src='../sdk.js';holder.appendChild(script);`)
	if len(f.inserted) != 0 {
		t.Fatal("disconnected resource notified the loader")
	}
	f.eval(t, `document.body.appendChild(holder);script.src='/next.js';script.text='changed';var base=document.createElement('base');base.setAttribute('href','https://cdn.fixture.test/version/');document.head.appendChild(base);var anchor=document.createElement('a');anchor.href='link';var frame=document.createElement('iframe');frame.src='frame';`)
	f.expect(t, `JSON.stringify({script:script.src,anchor:anchor.href,frame:frame.src})`, `{"script":"https://cdn.fixture.test/next.js","anchor":"https://cdn.fixture.test/version/link","frame":"https://cdn.fixture.test/version/frame"}`)
	var scriptNotifications int
	for _, node := range f.inserted {
		if node == f.vm.Get("script").ToObject(f.vm) {
			scriptNotifications++
		}
	}
	if scriptNotifications != 3 {
		t.Fatalf("script loader notifications=%d, want 3", scriptNotifications)
	}
}

func TestDOMRemovedConnectedSubtreeNotifiesHost(t *testing.T) {
	f := newDOMFixture(t)
	f.eval(t, `
var detached=document.createElement('div'),orphan=document.createElement('iframe');detached.appendChild(orphan);orphan.remove();
var container=document.createElement('div'),frame=document.createElement('iframe');container.appendChild(frame);document.body.appendChild(container);
frame.remove();container.appendChild(frame);container.remove();
`)
	if len(f.removed) != 2 || f.removed[0] != f.vm.Get("frame").ToObject(f.vm) || f.removed[1] != f.vm.Get("container").ToObject(f.vm) {
		t.Fatal("host removal bridge did not distinguish connected nodes and subtree roots")
	}
	// Standalone DOM hosts need not own frame or worker cleanup.
	f.eval(t, `delete __host.removed;document.body.appendChild(container);container.remove();`)
}

func TestDOMFrameCookieAndDocumentWrite(t *testing.T) {
	f := newDOMFixture(t)
	f.eval(t, `
var frames=new WeakMap();__host.frame=node=>{if(!frames.has(node)){const proxy={};Object.defineProperty(proxy,'document',{get(){if(node.src.startsWith('https://other.test/'))throw new DOMException('Blocked document','SecurityError');return document}});frames.set(node,proxy)}return frames.get(node)};
var frame=document.createElement('iframe'),before=frame.contentWindow;document.body.appendChild(frame);var stable=frame.contentWindow===frame.contentWindow&&frame.contentDocument===document;frame.src='https://other.test/frame';var hidden=frame.contentDocument===null;
var beforeCookie=document.cookie;document.cookie='fixture=written; Path=/';var afterCookie=document.cookie;
var writeEvents=[];document.addEventListener('DOMContentLoaded',e=>writeEvents.push('ready:'+e.isTrusted+':'+document.readyState));addEventListener('load',e=>writeEvents.push('load:'+e.isTrusted+':'+document.readyState));
document.open();document.write('<!doctype html><html><head><title>fixture</title></head><body><p id="written">');document.writeln('hello');document.write('</p></body></html>');document.close();
`)
	f.expect(t, `JSON.stringify({before,stable,hidden,beforeCookie,afterCookie,title:document.title,text:document.getElementById('written').textContent,ready:document.readyState,writeEvents})`, `{"before":null,"stable":true,"hidden":true,"beforeCookie":"visible=initial","afterCookie":"fixture=written; Path=/","title":"fixture","text":"hello\n","ready":"complete","writeEvents":["ready:true:interactive","load:true:complete"]}`)
	if len(f.cookieWrites) != 1 || f.cookieWrites[0] != "fixture=written; Path=/" {
		t.Fatalf("cookie bridge writes=%v", f.cookieWrites)
	}
}
