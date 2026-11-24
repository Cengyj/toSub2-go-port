package jshost

import "testing"

func TestOpaqueFrameDocumentsRemainIsolated(t *testing.T) {
	for _, parentURL := range []string{"https://fixture.test/", "data:text/html,identical"} {
		t.Run(parentURL, func(t *testing.T) {
			_, parent, _ := testRealm(t, parentURL)
			evalTest(t, parent, `var first=document.createElement('iframe'),second=document.createElement('iframe');
first.src=second.src='data:text/html,identical';document.body.appendChild(first);document.body.appendChild(second);`)
			child := parent.frames[parent.vm.Get("first").ToObject(parent.vm)]
			evalTest(t, child, `if (parent.frames[0].document!==document || frameElement!==null) throw Error('opaque document identity');
const sibling=parent.frames[1];
for (const action of [()=>sibling.document,()=>{sibling.secret='leak'},()=>{delete sibling.secret}]) {
  let denied=false;try {action()} catch(error) {denied=error.name==='SecurityError'}
  if(!denied) throw Error('separate opaque document exposed');
}
if('document' in sibling || Object.keys(sibling).includes('document')) throw Error('opaque properties exposed');
let parentDenied=false;try{parent.document}catch(error){parentDenied=error.name==='SecurityError'}
if(!parentDenied) throw Error('opaque child inherited explicit data URL origin');`)
		})
	}
}

func TestBlankFrameInheritsDocumentOrigin(t *testing.T) {
	for _, parentURL := range []string{"https://fixture.test/path", "data:text/html,parent"} {
		for _, source := range []string{"", "about:blank", "about:blank#fragment"} {
			t.Run(parentURL+"/"+source, func(t *testing.T) {
				_, parent, _ := testRealm(t, parentURL)
				if err := parent.vm.Set("frameSource", source); err != nil {
					t.Fatal(err)
				}
				evalTest(t, parent, `var frame=document.createElement('iframe');if(frameSource)frame.src=frameSource;
document.body.appendChild(frame);var child=frame.contentWindow;child.document.body.id='inherited';
if(child.frameElement!==frame || child.parent!==window)throw Error('inherited window identity');`)
				child := parent.frames[parent.vm.Get("frame").ToObject(parent.vm)]
				evalTest(t, child, `if(document.body.id!=='inherited'||parent.frame.contentDocument!==document||frameElement!==parent.frame)throw Error('inherited document access');`)
				if origin(parentURL) == "null" {
					evalTest(t, child, `let denied=false;try{localStorage}catch(error){denied=error.name==='SecurityError'}if(!denied)throw Error('opaque storage exposed');`)
				} else {
					evalTest(t, parent, `localStorage.setItem('fixture','shared');if(child.localStorage.getItem('fixture')!=='shared')throw Error('inherited storage separated');`)
				}
			})
		}
	}
}

func TestOpaqueAndInheritedWindowMessageOrigins(t *testing.T) {
	h, parent, _ := testRealm(t, "https://fixture.test/")
	evalTest(t, parent, `var first=document.createElement('iframe'),second=document.createElement('iframe'),blank=document.createElement('iframe');
first.src=second.src='data:text/html,same';blank.src='about:blank';
document.body.appendChild(first);document.body.appendChild(second);document.body.appendChild(blank);
var received=[];addEventListener('message',event=>received.push([event.data,event.origin,event.source===blank.contentWindow]));`)
	first := parent.frames[parent.vm.Get("first").ToObject(parent.vm)]
	second := parent.frames[parent.vm.Get("second").ToObject(parent.vm)]
	blank := parent.frames[parent.vm.Get("blank").ToObject(parent.vm)]
	evalTest(t, second, `var received=[];addEventListener('message',event=>received.push([event.data,event.origin,event.source===parent.frames[0]]));`)
	evalTest(t, first, `parent.frames[1].postMessage('discard');parent.frames[1].postMessage('opaque','*');`)
	evalTest(t, blank, `parent.postMessage('inherited');`)
	drainTasks(t, h)
	if got := evalTest(t, second, `JSON.stringify(received)`).String(); got != `[["opaque","null",true]]` {
		t.Fatal("opaque messages crossed origin or lost source identity:", got)
	}
	if got := evalTest(t, parent, `JSON.stringify(received)`).String(); got != `[["inherited","https://fixture.test",true]]` {
		t.Fatal("inherited origin was not used for messages:", got)
	}
}

func TestOpaqueFrameNavigationDoesNotReuseInheritedOrigin(t *testing.T) {
	h, parent, _ := testRealm(t, "data:text/html,parent")
	evalTest(t, parent, `var frame=document.createElement('iframe');document.body.appendChild(frame);
var proxy=frame.contentWindow;proxy.document.body.id='inherited';
frame.src=location.href;
if(frame.contentDocument!==null||proxy!==frame.contentWindow)throw Error('explicit data navigation retained inherited origin');
frame.removeAttribute('src');
if(frame.contentWindow!==proxy||!frame.contentDocument||frame.contentDocument.body.id)throw Error('blank navigation failed to inherit fresh document');`)
	drainTasks(t, h)
	if len(h.realms) != 4 || !h.realms[1].retired || !h.realms[2].retired {
		t.Fatal("origin-changing navigation did not retire both prior documents")
	}
}

func TestOpaqueFramesCannotUseSiblingBlobSources(t *testing.T) {
	h, parent, _ := testRealm(t, "https://fixture.test/")
	evalTest(t, parent, `var first=document.createElement('iframe'),second=document.createElement('iframe');
first.src=second.src='data:text/html,same';document.body.appendChild(first);document.body.appendChild(second);`)
	first := parent.frames[parent.vm.Get("first").ToObject(parent.vm)]
	second := parent.frames[parent.vm.Get("second").ToObject(parent.vm)]
	key := evalTest(t, first, `URL.createObjectURL(new Blob(['postMessage("private")']))`).String()
	if err := second.vm.Set("siblingBlob", key); err != nil {
		t.Fatal(err)
	}
	evalTest(t, second, `URL.revokeObjectURL(siblingBlob);`)
	if _, found := h.blobs[key]; !found {
		t.Fatal("opaque sibling revoked another document's blob")
	}
	evalTest(t, second, `let blocked=false;try{new Worker(siblingBlob)}catch(error){blocked=true}if(!blocked)throw Error('opaque sibling started private blob worker');`)
	if len(h.allWorkers) != 0 {
		t.Fatal("cross-origin blob worker was started")
	}
	if err := first.vm.Set("ownedBlob", key); err != nil {
		t.Fatal(err)
	}
	evalTest(t, first, `URL.revokeObjectURL(ownedBlob);`)
	if _, found := h.blobs[key]; found {
		t.Fatal("blob owner could not revoke its source")
	}
}

func TestOpaqueBlobFrameRetainsCreatorOrigin(t *testing.T) {
	_, parent, _ := testRealm(t, "data:text/html,parent")
	evalTest(t, parent, `var ownedURL=URL.createObjectURL(new Blob(['fixture']));
var owned=document.createElement('iframe'),unrelated=document.createElement('iframe');
owned.src=ownedURL;unrelated.src='data:text/html,sibling';
document.body.appendChild(owned);document.body.appendChild(unrelated);
owned.contentWindow.document.body.id='creator';
if(owned.contentWindow.frameElement!==owned)throw Error('blob creator identity lost');`)
	child := parent.frames[parent.vm.Get("unrelated").ToObject(parent.vm)]
	if err := child.vm.Set("publicBlobURL", evalTest(t, parent, "ownedURL").String()); err != nil {
		t.Fatal(err)
	}
	evalTest(t, child, `var foreign=document.createElement('iframe');foreign.src=publicBlobURL;document.body.appendChild(foreign);
if(foreign.contentDocument!==null)throw Error('blob inherited consumer origin');`)
}
