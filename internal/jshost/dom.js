(() => {
  "use strict";
  const realm = globalThis;
  const state = new WeakMap(), listeners = new WeakMap(), eventState = new WeakMap();
  const XHTML = "http://www.w3.org/1999/xhtml";
  const microtask = fn => queueMicrotask(fn);
  const report = error => microtask(() => { throw error; });
  let nextNodeID = 0;

  class DOMException extends Error {
    constructor(message = "", name = "Error") { super(String(message)); this.name = String(name); }
    get code() { return ({IndexSizeError:1,HierarchyRequestError:3,WrongDocumentError:4,InvalidCharacterError:5,NotFoundError:8,NotSupportedError:9,InvalidStateError:11,SyntaxError:12,InvalidModificationError:13,NamespaceError:14,SecurityError:18,NetworkError:19,AbortError:20,DataCloneError:25})[this.name] || 0; }
  }
  function fault(name, message) { throw new DOMException(message, name); }
  class Event {
    constructor(type, init = {}) {
      if (arguments.length === 0) throw new TypeError("Event type is required");
      eventState.set(this, {type:String(type),bubbles:!!init.bubbles,cancelable:!!init.cancelable,composed:!!init.composed,target:null,currentTarget:null,phase:0,trusted:false,canceled:false,stopped:false,immediate:false,dispatching:false,passive:false,path:[],timeStamp:typeof performance === "object" ? performance.now() : Date.now()});
      Object.defineProperty(this,"isTrusted",{get:()=>eventState.get(this).trusted,enumerable:true});
    }
    get type() { return eventState.get(this).type; }
    get bubbles() { return eventState.get(this).bubbles; }
    get cancelable() { return eventState.get(this).cancelable; }
    get composed() { return eventState.get(this).composed; }
    get target() { return eventState.get(this).target; }
    get srcElement() { return this.target; }
    get currentTarget() { return eventState.get(this).currentTarget; }
    get eventPhase() { return eventState.get(this).phase; }
    get defaultPrevented() { return eventState.get(this).canceled; }
    get timeStamp() { return eventState.get(this).timeStamp; }
    get cancelBubble() { return eventState.get(this).stopped; }
    set cancelBubble(value) { if (value) this.stopPropagation(); }
    get returnValue() { return !this.defaultPrevented; }
    set returnValue(value) { if (!value) this.preventDefault(); }
    stopPropagation() { eventState.get(this).stopped = true; }
    stopImmediatePropagation() { const s=eventState.get(this); s.stopped=true; s.immediate=true; }
    preventDefault() { const s=eventState.get(this); if(s.cancelable&&!s.passive)s.canceled=true; }
    composedPath() { return eventState.get(this).path.slice(); }
    initEvent(type, bubbles = false, cancelable = false) { const s=eventState.get(this); if(s.dispatching)return; Object.assign(s,{type:String(type),bubbles:!!bubbles,cancelable:!!cancelable,target:null,trusted:false,canceled:false,stopped:false,immediate:false}); }
  }
  for (const [name,value] of Object.entries({NONE:0,CAPTURING_PHASE:1,AT_TARGET:2,BUBBLING_PHASE:3})) {
    Object.defineProperty(Event,name,{value}); Object.defineProperty(Event.prototype,name,{value});
  }
  class MessageEvent extends Event {
    constructor(type, init = {}) { super(type,init); this.data=init.data === undefined ? null : init.data; this.origin=String(init.origin || ""); this.lastEventId=String(init.lastEventId || ""); this.source=init.source || null; this.ports=Array.from(init.ports || []); }
  }
  class CustomEvent extends Event { constructor(type,init={}) { super(type,init); this.detail=init.detail === undefined ? null : init.detail; } initCustomEvent(type,bubbles,cancelable,detail) { this.initEvent(type,bubbles,cancelable); this.detail=detail; } }
  class ErrorEvent extends Event { constructor(type,init={}) { super(type,init); this.message=String(init.message||"");this.filename=String(init.filename||"");this.lineno=Number(init.lineno||0);this.colno=Number(init.colno||0);this.error=init.error===undefined?null:init.error; } }
  class ProgressEvent extends Event { constructor(type,init={}) { super(type,init);this.lengthComputable=!!init.lengthComputable;this.loaded=Number(init.loaded||0);this.total=Number(init.total||0); } }
  class MouseEvent extends Event { constructor(type,init={}) { super(type,init); for(const key of ["screenX","screenY","clientX","clientY","button","buttons","detail"])this[key]=Number(init[key]||0);for(const key of ["ctrlKey","shiftKey","altKey","metaKey"])this[key]=!!init[key];this.relatedTarget=init.relatedTarget||null;this.view=init.view||null; } }
  class EventTarget {
    addEventListener(type, callback, options = {}) {
      if (callback == null) return;
      if (typeof callback !== "function" && (typeof callback !== "object" || typeof callback.handleEvent !== "function")) return;
      type=String(type); options=typeof options === "boolean" ? {capture:options} : options || {};
      if(options.signal?.aborted)return;
      let list=listeners.get(this);if(!list)listeners.set(this,list=new Map());
      let records=list.get(type);if(!records)list.set(type,records=[]);
      if(records.some(r=>r.callback===callback&&r.capture===!!options.capture))return;
      const record={callback,capture:!!options.capture,once:!!options.once,passive:!!options.passive,removed:false};records.push(record);
      if(options.signal)options.signal.addEventListener("abort",()=>this.removeEventListener(type,callback,record.capture),{once:true});
    }
    removeEventListener(type, callback, options = {}) {
      const capture=typeof options === "boolean" ? options : !!options?.capture;
      const records=listeners.get(this)?.get(String(type));if(!records)return;
      for(const record of records)if(record.callback===callback&&record.capture===capture)record.removed=true;
      listeners.get(this).set(String(type),records.filter(r=>!r.removed));
    }
    dispatchEvent(event) { return dispatch(this,event,false); }
  }
  function dispatch(target,event,isTrusted) {
      const s=eventState.get(event);if(!s)throw new TypeError("Expected an Event");
      if(s.dispatching||!s.type)fault("InvalidStateError","Event is already being dispatched or has no type");
      s.dispatching=true;s.target=target;s.trusted=isTrusted;s.stopped=false;s.immediate=false;
      const path=[target];let node=target;
      while(state.has(node)&&node.parentNode){node=node.parentNode;path.push(node);}
      if(state.has(node)&&node.nodeType===9&&node.defaultView)path.push(node.defaultView);
      s.path=path.slice();
      const invoke=(target,capture,phase)=>{
        s.currentTarget=target;s.phase=phase;
        for(const record of (listeners.get(target)?.get(s.type)||[]).slice()){
          if(record.removed||record.capture!==capture)continue;
          if(record.once)EventTarget.prototype.removeEventListener.call(target,s.type,record.callback,record.capture);
          s.passive=record.passive;
          try{if(typeof record.callback==="function")record.callback.call(target,event);else record.callback.handleEvent.call(record.callback,event);}catch(error){report(error);}
          s.passive=false;if(s.immediate)break;
        }
        if(!capture&&!s.immediate&&typeof target["on"+s.type]==="function"){
          try{if(target["on"+s.type].call(target,event)===false)event.preventDefault();}catch(error){report(error);}
        }
      };
      try {
        for(let i=path.length-1;i>0&&!s.stopped;i--)invoke(path[i],true,1);
        if(!s.stopped){invoke(target,true,2);if(!s.immediate)invoke(target,false,2);}
        if(s.bubbles&&!s.stopped)for(let i=1;i<path.length&&!s.stopped;i++)invoke(path[i],false,3);
        return !s.canceled;
      } finally {s.currentTarget=null;s.phase=0;s.dispatching=false;s.passive=false;s.path=[];}
  }
  class NodeList {
    constructor(items) { Object.defineProperty(this,"_items",{value:typeof items==="function"?items:()=>items}); return indexed(this); }
    get length() { return this._items().length; }
    item(index) { return this._items()[Number(index)] || null; }
    forEach(fn,thisArg) { this._items().slice().forEach((value,index)=>fn.call(thisArg,value,index,this)); }
    entries() { return this._items().slice().entries(); }
    keys() { return this._items().slice().keys(); }
    values() { return this._items().slice().values(); }
    [Symbol.iterator]() { return this.values(); }
  }
  class HTMLCollection extends NodeList { namedItem(name) { name=String(name);return this._items().find(n=>n.id===name||n.getAttribute("name")===name)||null; } }
  function indexed(target) { return new Proxy(target,{get(object,key,receiver){if(typeof key==="string"&&/^(0|[1-9][0-9]*)$/.test(key))return object._items()[Number(key)];return Reflect.get(object,key,receiver);},has(object,key){return typeof key==="string"&&/^(0|[1-9][0-9]*)$/.test(key)?Number(key)<object.length:Reflect.has(object,key);},ownKeys(object){return [...object._items().keys()].map(String).concat(Reflect.ownKeys(object));},getOwnPropertyDescriptor(object,key){if(typeof key==="string"&&/^(0|[1-9][0-9]*)$/.test(key)&&Number(key)<object.length)return {configurable:true,enumerable:true,value:object._items()[Number(key)],writable:false};return Reflect.getOwnPropertyDescriptor(object,key);}}); }

  const observers=new Set();let mutationsQueued=false;
  function mutation(record) {
    for(const observer of observers){
      const matches=[...observer._observations,...observer._transient].filter(o=>o.target===record.target||(o.options.subtree&&o.target.contains(record.target)));
      if(record.type==="childList")for(const observation of matches)if(observation.options.subtree)for(const target of record.removedNodes||[])observer._transient.push({target,options:observation.options});
      const observations=matches.filter(o=>record.type==="childList"?o.options.childList:record.type==="attributes"?o.options.attributes&&(!o.options.attributeFilter||o.options.attributeFilter.includes(record.attributeName)):o.options.characterData);
      if(!observations.length)continue;
      const copy={...record,addedNodes:new NodeList(record.addedNodes||[]),removedNodes:new NodeList(record.removedNodes||[]),previousSibling:record.previousSibling||null,nextSibling:record.nextSibling||null,attributeName:record.attributeName||null,attributeNamespace:record.attributeNamespace||null,oldValue:null};
      if(observations.some(o=>record.type==="attributes"?o.options.attributeOldValue:record.type==="characterData"&&o.options.characterDataOldValue))copy.oldValue=record.oldValue;
      observer._records.push(copy);
    }
    if(!mutationsQueued&&[...observers].some(o=>o._records.length||o._transient.length)){
      mutationsQueued=true;microtask(()=>{mutationsQueued=false;for(const observer of [...observers]){const records=observer.takeRecords();observer._transient=[];if(records.length)try{observer._callback(records,observer);}catch(error){report(error);}}});
    }
  }
  class MutationObserver {
    constructor(callback) { if(typeof callback!=="function")throw new TypeError("Mutation callback must be callable");this._callback=callback;this._records=[];this._observations=[];this._transient=[]; }
    observe(target,options={}) {
      if(!state.has(target))throw new TypeError("Mutation target must be a Node");
      options={...options};if(options.attributes===undefined&&(options.attributeOldValue||options.attributeFilter))options.attributes=true;if(options.characterData===undefined&&options.characterDataOldValue)options.characterData=true;
      if(!options.childList&&!options.attributes&&!options.characterData||options.attributeOldValue&&!options.attributes||options.attributeFilter&&!options.attributes||options.characterDataOldValue&&!options.characterData)throw new TypeError("Invalid mutation observation options");
      if(options.attributeFilter)options.attributeFilter=Array.from(options.attributeFilter,String);
      this._observations=this._observations.filter(o=>o.target!==target);this._observations.push({target,options});observers.add(this);
    }
    disconnect() { this._observations=[];this._transient=[];this._records=[];observers.delete(this); }
    takeRecords() { const records=this._records;this._records=[];return records; }
  }
  function childrenOf(node) { return state.get(node).children; }
  function requireNode(node) { if(!state.has(node))throw new TypeError("Expected a Node");return node; }
  function adopt(node,document) { state.get(node).owner=document;for(const child of childrenOf(node))adopt(child,document); }
  function connected(node) { for(let n=node;n;n=n.parentNode)if(n.nodeType===9)return true;return false; }
  function notifyInserted(node) { if(!node.isConnected)return;__host.inserted(node);for(const child of childrenOf(node))notifyInserted(child); }
  function descendants(node) { const result=[];for(const child of childrenOf(node)){result.push(child);result.push(...descendants(child));}return result; }
  function validateInsertion(parent,node,reference,replaced=null) {
    requireNode(node);
    if(reference!=null){requireNode(reference);if(reference.parentNode!==parent)fault("NotFoundError","Reference is not a child");}
    if(![1,9,11].includes(parent.nodeType)||node===parent||node.contains(parent)||![1,3,8,10,11].includes(node.nodeType))fault("HierarchyRequestError","Invalid tree insertion");
    const incoming=node.nodeType===11?Array.from(node.childNodes):[node];
    if(parent.nodeType!==9){if(incoming.some(n=>n.nodeType===10))fault("HierarchyRequestError","Doctype requires a Document");return;}
    const remaining=childrenOf(parent).filter(n=>n!==replaced&&!incoming.includes(n));
    const position=reference==null?remaining.length:childrenOf(parent).slice(0,childrenOf(parent).indexOf(reference)).filter(n=>n!==replaced&&!incoming.includes(n)).length;
    remaining.splice(position,0,...incoming);
    const elements=remaining.filter(n=>n.nodeType===1),doctypes=remaining.filter(n=>n.nodeType===10);
    if(remaining.some(n=>n.nodeType===3)||elements.length>1||doctypes.length>1||(elements.length&&doctypes.length&&remaining.indexOf(doctypes[0])>remaining.indexOf(elements[0])))fault("HierarchyRequestError","Invalid Document children");
  }
  class Node extends EventTarget {
    constructor(type=0,name="",owner=null) { super();state.set(this,{type,name,owner,parent:null,children:[],data:"",attributes:new Map()});Object.defineProperty(this,"__nodeId",{value:++nextNodeID}); }
    get nodeType() { return state.get(this).type; }
    get nodeName() { return state.get(this).name; }
    get ownerDocument() { return this.nodeType===9?null:state.get(this).owner; }
    get parentNode() { return state.get(this).parent; }
    get parentElement() { return this.parentNode?.nodeType===1?this.parentNode:null; }
    get childNodes() { const s=state.get(this);return s.list||(s.list=new NodeList(()=>s.children)); }
    get children() { const s=state.get(this);return s.collection||(s.collection=new HTMLCollection(()=>s.children.filter(n=>n.nodeType===1))); }
    get firstChild() { return childrenOf(this)[0]||null; }
    get lastChild() { return childrenOf(this).at(-1)||null; }
    get firstElementChild() { return Array.from(this.children)[0]||null; }
    get lastElementChild() { return Array.from(this.children).at(-1)||null; }
    get childElementCount() { return this.children.length; }
    get previousSibling() { if(!this.parentNode)return null;const children=childrenOf(this.parentNode);return children[children.indexOf(this)-1]||null; }
    get nextSibling() { if(!this.parentNode)return null;const children=childrenOf(this.parentNode);return children[children.indexOf(this)+1]||null; }
    get previousElementSibling() { let node=this.previousSibling;while(node&&node.nodeType!==1)node=node.previousSibling;return node; }
    get nextElementSibling() { let node=this.nextSibling;while(node&&node.nodeType!==1)node=node.nextSibling;return node; }
    get isConnected() { return connected(this); }
    get baseURI() { const document=this.nodeType===9?this:this.ownerDocument;const base=document?.querySelector("base[href]");return base?__host.resolveURL(base.getAttribute("href"),document.URL):document?.URL||__config.url; }
    get nodeValue() { return this.nodeType===3||this.nodeType===8?state.get(this).data:null; }
    set nodeValue(value) { if(this.nodeType===3||this.nodeType===8)this.data=value==null?"":String(value); }
    get textContent() { if(this.nodeType===9||this.nodeType===10)return null;if(this.nodeType===3||this.nodeType===8)return this.data;return childrenOf(this).filter(n=>n.nodeType!==8).map(n=>n.textContent||"").join(""); }
    set textContent(value) { if(this.nodeType===9||this.nodeType===10)return;if(this.nodeType===3||this.nodeType===8){this.data=value==null?"":String(value);return;}this.replaceChildren();if(value!=null&&String(value)!=="")this.appendChild((this.ownerDocument||this).createTextNode(String(value))); }
    hasChildNodes() { return !!childrenOf(this).length; }
    contains(other) { for(let node=other;state.has(node);node=node.parentNode){if(node===this)return true;if(!node.parentNode)break;}return false; }
    getRootNode() { let node=this;while(node.parentNode)node=node.parentNode;return node; }
    appendChild(node) { return this.insertBefore(node,null); }
    insertBefore(node,reference) {
      validateInsertion(this,node,reference);
      if(node===reference)return node;
      if(node.nodeType===11){for(const child of Array.from(node.childNodes))this.insertBefore(child,reference);return node;}
      if(node.parentNode)node.parentNode.removeChild(node);
      const children=childrenOf(this),index=reference==null?children.length:children.indexOf(reference),previous=children[index-1]||null;
      children.splice(index,0,node);state.get(node).parent=this;adopt(node,this.nodeType===9?this:this.ownerDocument);
      mutation({type:"childList",target:this,addedNodes:[node],removedNodes:[],previousSibling:previous,nextSibling:reference||null});
      notifyInserted(node);if(this.nodeType===1&&this.localName==="script"&&this.isConnected)__host.inserted(this);
      return node;
    }
    removeChild(node) { requireNode(node);if(node.parentNode!==this)fault("NotFoundError","Node is not a child");const wasConnected=node.isConnected,previous=node.previousSibling,next=node.nextSibling;childrenOf(this).splice(childrenOf(this).indexOf(node),1);state.get(node).parent=null;mutation({type:"childList",target:this,addedNodes:[],removedNodes:[node],previousSibling:previous,nextSibling:next});if(wasConnected)__host.removed?.(node);return node; }
    replaceChild(node,old) { requireNode(old);if(old.parentNode!==this)fault("NotFoundError","Node is not a child");if(node===old)return old;validateInsertion(this,node,old,old);let reference=old.nextSibling;if(reference===node)reference=node.nextSibling;this.removeChild(old);this.insertBefore(node,reference);return old; }
    append(...nodes) { for(const node of nodes)this.appendChild(state.has(node)?node:(this.ownerDocument||this).createTextNode(String(node))); }
    prepend(...nodes) { const reference=this.firstChild;for(const node of nodes)this.insertBefore(state.has(node)?node:(this.ownerDocument||this).createTextNode(String(node)),reference); }
    replaceChildren(...nodes) { for(const node of Array.from(this.childNodes))this.removeChild(node);this.append(...nodes); }
    remove() { if(this.parentNode)this.parentNode.removeChild(this); }
    before(...nodes) { if(!this.parentNode)return;for(const node of nodes)this.parentNode.insertBefore(state.has(node)?node:this.ownerDocument.createTextNode(String(node)),this); }
    after(...nodes) { if(!this.parentNode)return;const reference=this.nextSibling;for(const node of nodes)this.parentNode.insertBefore(state.has(node)?node:this.ownerDocument.createTextNode(String(node)),reference); }
    replaceWith(...nodes) { if(this.parentNode){this.before(...nodes);this.remove();} }
    cloneNode(deep=false) { const clone=cloneShallow(this,this.ownerDocument);if(deep)for(const child of this.childNodes)clone.appendChild(child.cloneNode(true));return clone; }
    isSameNode(other) { return this===other; }
    isEqualNode(other) { return state.has(other)&&this.nodeType===other.nodeType&&this.nodeName===other.nodeName&&this.nodeValue===other.nodeValue&&(!this.attributes||JSON.stringify([...state.get(this).attributes])===JSON.stringify([...state.get(other).attributes]))&&this.childNodes.length===other.childNodes.length&&Array.from(this.childNodes).every((child,index)=>child.isEqualNode(other.childNodes[index])); }
    compareDocumentPosition(other) { requireNode(other);if(other===this)return 0;if(this.getRootNode()!==other.getRootNode())return 1|32|(this.__nodeId<other.__nodeId?4:2);if(this.contains(other))return 4|16;if(other.contains(this))return 2|8;const nodes=[this.getRootNode(),...descendants(this.getRootNode())];return nodes.indexOf(this)<nodes.indexOf(other)?4:2; }
    normalize() { for(const child of Array.from(this.childNodes)){if(child.parentNode!==this)continue;if(child.nodeType===3){while(child.nextSibling?.nodeType===3){child.data+=child.nextSibling.data;this.removeChild(child.nextSibling);}if(!child.data)this.removeChild(child);}else child.normalize();} }
  }
  for(const[name,value]of Object.entries({ELEMENT_NODE:1,ATTRIBUTE_NODE:2,TEXT_NODE:3,COMMENT_NODE:8,DOCUMENT_NODE:9,DOCUMENT_TYPE_NODE:10,DOCUMENT_FRAGMENT_NODE:11,DOCUMENT_POSITION_DISCONNECTED:1,DOCUMENT_POSITION_PRECEDING:2,DOCUMENT_POSITION_FOLLOWING:4,DOCUMENT_POSITION_CONTAINS:8,DOCUMENT_POSITION_CONTAINED_BY:16,DOCUMENT_POSITION_IMPLEMENTATION_SPECIFIC:32})){Object.defineProperty(Node,name,{value});Object.defineProperty(Node.prototype,name,{value});}
  class CharacterData extends Node {
    get data() { return state.get(this).data; }
    set data(value) { const oldValue=this.data;state.get(this).data=String(value);mutation({type:"characterData",target:this,oldValue}); }
    get length() { return this.data.length; }
    substringData(offset,count) { offset=Number(offset);if(offset<0||offset>this.length)fault("IndexSizeError","Invalid character offset");return this.data.substring(offset,offset+Number(count)); }
    appendData(value) { this.data+=String(value); }
    insertData(offset,value) { this.replaceData(offset,0,value); }
    deleteData(offset,count) { this.replaceData(offset,count,""); }
    replaceData(offset,count,value) { offset=Number(offset);if(offset<0||offset>this.length)fault("IndexSizeError","Invalid character offset");this.data=this.data.slice(0,offset)+String(value)+this.data.slice(offset+Number(count)); }
  }
  class Text extends CharacterData { constructor(data="",owner=null) { super(3,"#text",owner);state.get(this).data=String(data); } splitText(offset) { const tail=new Text(this.substringData(offset,this.length),this.ownerDocument);this.data=this.data.slice(0,offset);if(this.parentNode)this.parentNode.insertBefore(tail,this.nextSibling);return tail; } get wholeText() { let first=this;while(first.previousSibling?.nodeType===3)first=first.previousSibling;let result="";while(first?.nodeType===3){result+=first.data;first=first.nextSibling;}return result; } }
  class Comment extends CharacterData { constructor(data="",owner=null) { super(8,"#comment",owner);state.get(this).data=String(data); } }
  class DocumentType extends Node { constructor(name="html",owner=null) { super(10,String(name),owner);this.name=String(name);this.publicId="";this.systemId=""; } }
  class Attr extends Node { constructor(name,value="",element=null) { super(2,String(name),element?.ownerDocument||null);this.name=String(name);this.ownerElement=element;this._value=String(value);this.namespaceURI=null;this.prefix=null;this.localName=this.name; } get value() { return this.ownerElement?this.ownerElement.getAttribute(this.name):this._value; } set value(value) { if(this.ownerElement)this.ownerElement.setAttribute(this.name,value);else this._value=String(value); } get nodeValue() { return this.value; } set nodeValue(value) { this.value=value; } get textContent() { return this.value; } set textContent(value) { this.value=value; } get specified() { return true; } }
  class NamedNodeMap extends NodeList { constructor(element) { super(()=>[...state.get(element).attributes].map(([name,value])=>new Attr(name,value,element)));Object.defineProperty(this,"_element",{value:element}); } getNamedItem(name) { return this._element.hasAttribute(name)?new Attr(name,this._element.getAttribute(name),this._element):null; } setNamedItem(attr) { if(!(attr instanceof Attr))throw new TypeError("Expected an Attr");const previous=this.getNamedItem(attr.name);this._element.setAttribute(attr.name,attr.value);attr.ownerElement=this._element;return previous; } removeNamedItem(name) { const old=this.getNamedItem(name);if(!old)fault("NotFoundError","Attribute does not exist");old._value=old.value;old.ownerElement=null;this._element.removeAttribute(name);return old; } getNamedItemNS(namespace,name) { return this.getNamedItem(name); } }
  class DOMTokenList {
    constructor(element,attribute) { Object.defineProperties(this,{_element:{value:element},_attribute:{value:attribute}});return new Proxy(this,{get:(target,key,receiver)=>typeof key==="string"&&/^\d+$/.test(key)?target._tokens()[Number(key)]:Reflect.get(target,key,receiver)}); }
    _tokens() { return [...new Set((this._element.getAttribute(this._attribute)||"").split(/\s+/).filter(Boolean))]; }
    _validate(token) { token=String(token);if(!token)fault("SyntaxError","Token is empty");if(/\s/.test(token))fault("InvalidCharacterError","Token contains whitespace");return token; }
    get length() { return this._tokens().length; }
    get value() { return this._element.getAttribute(this._attribute)||""; }
    set value(value) { this._element.setAttribute(this._attribute,String(value)); }
    item(index) { return this._tokens()[Number(index)]||null; }
    contains(token) { return this._tokens().includes(this._validate(token)); }
    add(...tokens) { tokens=tokens.map(t=>this._validate(t));this.value=[...new Set(this._tokens().concat(tokens))].join(" "); }
    remove(...tokens) { tokens=tokens.map(t=>this._validate(t));this.value=this._tokens().filter(t=>!tokens.includes(t)).join(" "); }
    toggle(token,force) { token=this._validate(token);const exists=this.contains(token);const add=force===undefined?!exists:!!force;if(add&&!exists)this.add(token);if(!add&&exists)this.remove(token);return add; }
    replace(old,newToken) { old=this._validate(old);newToken=this._validate(newToken);const tokens=this._tokens(),index=tokens.indexOf(old);if(index<0)return false;tokens[index]=newToken;this.value=[...new Set(tokens)].join(" ");return true; }
    forEach(fn,thisArg) { this._tokens().forEach((token,index)=>fn.call(thisArg,token,index,this)); }
    entries() { return this._tokens().entries(); } keys() { return this._tokens().keys(); } values() { return this._tokens().values(); } [Symbol.iterator]() { return this.values(); } toString() { return this.value; }
  }
  function cssName(name) { return name==="cssFloat"?"float":name.startsWith("--")?name:name.replace(/[A-Z]/g,c=>"-"+c.toLowerCase()); }
  class CSSStyleDeclaration {
    constructor(element) { Object.defineProperty(this,"_element",{value:element});return new Proxy(this,{get:(target,key,receiver)=>typeof key==="string"&&!Reflect.has(target,key)?/^\d+$/.test(key)?target.item(Number(key)):target.getPropertyValue(cssName(key)):Reflect.get(target,key,receiver),set:(target,key,value,receiver)=>typeof key==="string"&&!Reflect.has(target,key)?(target.setProperty(cssName(key),value),true):Reflect.set(target,key,value,receiver)}); }
    _properties() { const result=new Map();for(const entry of (this._element.getAttribute("style")||"").split(";")){const colon=entry.indexOf(":");if(colon<0)continue;const name=entry.slice(0,colon).trim().toLowerCase(),raw=entry.slice(colon+1).trim(),important=/\s*!important\s*$/i.test(raw);if(name)result.set(name,{value:raw.replace(/\s*!important\s*$/i,""),priority:important?"important":""});}return result; }
    get cssText() { return this._element.getAttribute("style")||""; } set cssText(value) { this._element.setAttribute("style",String(value)); }
    get length() { return this._properties().size; } item(index) { return [...this._properties().keys()][index]||""; }
    getPropertyValue(name) { return this._properties().get(String(name).toLowerCase())?.value||""; }
    getPropertyPriority(name) { return this._properties().get(String(name).toLowerCase())?.priority||""; }
    setProperty(name,value,priority="") { name=String(name).toLowerCase();value=String(value);priority=String(priority).toLowerCase();if(priority&&priority!=="important")return;const properties=this._properties();if(value)properties.set(name,{value,priority});else properties.delete(name);this.cssText=[...properties].map(([name,p])=>name+": "+p.value+(p.priority?" !important":"")+";").join(" "); }
    removeProperty(name) { const previous=this.getPropertyValue(name);this.setProperty(name,"");return previous; }
  }
  function attributeName(element,name) { name=String(name);return element.namespaceURI===XHTML?name.toLowerCase():name; }
  class Element extends Node {
    constructor(tag="div",owner=null,namespace=XHTML) { tag=String(tag);super(1,namespace===XHTML?tag.toUpperCase():tag,owner);state.get(this).tag=namespace===XHTML?tag.toLowerCase():tag;state.get(this).namespace=namespace; }
    get localName() { return state.get(this).tag; } get tagName() { return this.nodeName; } get namespaceURI() { return state.get(this).namespace; } get prefix() { return null; }
    get attributes() { const s=state.get(this);return s.attributeList||(s.attributeList=new NamedNodeMap(this)); }
    getAttribute(name) { return state.get(this).attributes.get(attributeName(this,name))??null; }
    hasAttribute(name) { return state.get(this).attributes.has(attributeName(this,name)); }
    getAttributeNames() { return [...state.get(this).attributes.keys()]; }
    setAttribute(name,value) { name=attributeName(this,name);if(!name||/[\s\u0000"'<>/=]/.test(name))fault("InvalidCharacterError","Invalid attribute name");const oldValue=this.getAttribute(name);state.get(this).attributes.set(name,String(value));mutation({type:"attributes",target:this,attributeName:name,oldValue});if(this.isConnected&&["src","srcdoc","type"].includes(name))__host.inserted(this); }
    removeAttribute(name) { name=attributeName(this,name);const oldValue=this.getAttribute(name);if(oldValue===null)return;state.get(this).attributes.delete(name);mutation({type:"attributes",target:this,attributeName:name,oldValue});if(this.isConnected&&["src","srcdoc","type"].includes(name))__host.inserted(this); }
    toggleAttribute(name,force) { const present=this.hasAttribute(name),add=force===undefined?!present:!!force;if(add&&!present)this.setAttribute(name,"");if(!add&&present)this.removeAttribute(name);return add; }
    getAttributeNode(name) { return this.attributes.getNamedItem(name); } setAttributeNode(attr) { return this.attributes.setNamedItem(attr); } removeAttributeNode(attr) { if(attr.ownerElement!==this)fault("NotFoundError","Attribute belongs to another element");return this.attributes.removeNamedItem(attr.name); }
    getAttributeNS(namespace,name) { return this.getAttribute(name); } hasAttributeNS(namespace,name) { return this.hasAttribute(name); } setAttributeNS(namespace,name,value) { this.setAttribute(name,value); } removeAttributeNS(namespace,name) { this.removeAttribute(name); }
    get id() { return this.getAttribute("id")||""; } set id(value) { this.setAttribute("id",value); }
    get className() { return this.getAttribute("class")||""; } set className(value) { this.setAttribute("class",value); }
    get classList() { const s=state.get(this);return s.classList||(s.classList=new DOMTokenList(this,"class")); }
    get innerHTML() { return childrenOf(this).map(n=>serialize(n,this.localName)).join(""); }
    set innerHTML(value) { const fragment=parseFragment(String(value),this.ownerDocument,this.localName);this.replaceChildren(...Array.from(fragment.childNodes)); }
    get outerHTML() { return serialize(this,""); }
    set outerHTML(value) { if(!this.parentNode)return;const fragment=parseFragment(String(value),this.ownerDocument,this.parentElement?.localName||"body");this.replaceWith(...Array.from(fragment.childNodes)); }
    insertAdjacentHTML(position,html) { position=String(position).toLowerCase();const outside=position==="beforebegin"||position==="afterend";const fragment=parseFragment(String(html),this.ownerDocument,outside?this.parentElement?.localName||"body":this.localName);this.insertAdjacentNodes(position,Array.from(fragment.childNodes)); }
    insertAdjacentElement(position,element) { requireNode(element);if(element.nodeType!==1)throw new TypeError("Expected an Element");return this.insertAdjacentNodes(position,[element])?element:null; }
    insertAdjacentText(position,text) { this.insertAdjacentNodes(position,[this.ownerDocument.createTextNode(String(text))]); }
    insertAdjacentNodes(position,nodes) { position=String(position).toLowerCase();if(position==="beforebegin"||position==="afterend"){if(!this.parentNode)return false;const reference=position==="beforebegin"?this:this.nextSibling;for(const node of nodes)this.parentNode.insertBefore(node,reference);}else if(position==="afterbegin"){const before=this.firstChild;for(const node of nodes)this.insertBefore(node,before);}else if(position==="beforeend")this.append(...nodes);else fault("SyntaxError","Invalid insertion position");return true; }
    querySelector(selector) { return select(this,selector)[0]||null; } querySelectorAll(selector) { return new NodeList(select(this,selector)); }
    matches(selector) { return parseSelector(selector).some(group=>matchesComplex(this,group,this)); }
    closest(selector) { const groups=parseSelector(selector);for(let node=this;node;node=node.parentElement)if(groups.some(group=>matchesComplex(node,group,this)))return node;return null; }
    getElementsByTagName(tag) { tag=String(tag).toLowerCase();return new HTMLCollection(()=>descendants(this).filter(n=>n.nodeType===1&&(tag==="*"||n.localName.toLowerCase()===tag))); }
    getElementsByClassName(names) { const tokens=String(names).split(/\s+/).filter(Boolean);return new HTMLCollection(()=>tokens.length?descendants(this).filter(n=>n.nodeType===1&&tokens.every(t=>n.classList.contains(t))):[]); }
  }
  class HTMLElement extends Element {
    get style() { const s=state.get(this);return s.style||(s.style=new CSSStyleDeclaration(this)); } set style(value) { this.style.cssText=String(value); }
    get dataset() { const s=state.get(this);if(s.dataset)return s.dataset;const element=this,convert=key=>"data-"+String(key).replace(/[A-Z]/g,c=>"-"+c.toLowerCase());return s.dataset=new Proxy({}, {get(target,key){if(typeof key!=="string")return Reflect.get(target,key);return element.getAttribute(convert(key))??undefined;},set(target,key,value){if(/-[a-z]/.test(String(key)))fault("SyntaxError","Invalid dataset property");element.setAttribute(convert(key),String(value));return true;},deleteProperty(target,key){element.removeAttribute(convert(key));return true;},ownKeys(){return element.getAttributeNames().filter(n=>n.startsWith("data-")).map(n=>n.slice(5).replace(/-([a-z])/g,(_,c)=>c.toUpperCase()));},getOwnPropertyDescriptor(target,key){const value=element.getAttribute(convert(key));return value===null?undefined:{value,writable:true,enumerable:true,configurable:true};},has(target,key){return element.hasAttribute(convert(key));}}); }
    get hidden() { return this.hasAttribute("hidden"); } set hidden(value) { this.toggleAttribute("hidden",!!value); }
    get innerText() { return this.textContent; } set innerText(value) { this.textContent=value; }
    get title() { return this.getAttribute("title")||""; } set title(value) { this.setAttribute("title",value); }
    get tabIndex() { return this.hasAttribute("tabindex")?Number(this.getAttribute("tabindex")): ["input","button","select","textarea","a"].includes(this.localName)?0:-1; } set tabIndex(value) { this.setAttribute("tabindex",Number(value)); }
    click() { this.dispatchEvent(new MouseEvent("click",{bubbles:true,cancelable:true,view:realm})); }
    focus() { this.ownerDocument._activeElement=this;this.dispatchEvent(new Event("focus")); }
    blur() { if(this.ownerDocument.activeElement===this)this.ownerDocument._activeElement=null;this.dispatchEvent(new Event("blur")); }
  }
  class HTMLScriptElement extends HTMLElement {
    constructor(owner=null) { super("script",owner);state.get(this).forceAsync=true; }
    get src() { const value=this.getAttribute("src");return value===null?"":__host.resolveURL(value,this.baseURI); } set src(value) { this.setAttribute("src",value); }
    get text() { return this.textContent; } set text(value) { this.textContent=value; }
    get async() { return this.hasAttribute("async")||state.get(this).forceAsync; } set async(value) { state.get(this).forceAsync=false;this.toggleAttribute("async",!!value); }
  }
  class HTMLIFrameElement extends HTMLElement {
    constructor(owner=null) { super("iframe",owner); }
    get src() { const value=this.getAttribute("src");return value===null?"":__host.resolveURL(value,this.baseURI); } set src(value) { this.setAttribute("src",value); }
    get srcdoc() { return this.getAttribute("srcdoc")||""; } set srcdoc(value) { this.setAttribute("srcdoc",value); }
    get contentWindow() { return this.isConnected?__host.frame(this):null; }
    get contentDocument() { const window=this.contentWindow;if(!window)return null;try{return window.document||null;}catch(error){if(error?.name==="SecurityError")return null;throw error;} }
    get sandbox() { const s=state.get(this);return s.sandbox||(s.sandbox=new DOMTokenList(this,"sandbox")); }
  }
  class HTMLAnchorElement extends HTMLElement { constructor(owner=null) { super("a",owner); } get href() { const value=this.getAttribute("href");return value===null?"":__host.resolveURL(value,this.baseURI); } set href(value) { this.setAttribute("href",value); } toString() { return this.href; } }
  class HTMLInputElement extends HTMLElement { constructor(owner=null) { super("input",owner); } get value() { return state.get(this).value??this.getAttribute("value")??""; } set value(value) { state.get(this).value=String(value); } get checked() { return state.get(this).checked??this.hasAttribute("checked"); } set checked(value) { state.get(this).checked=!!value; } }
  class HTMLBodyElement extends HTMLElement { constructor(owner=null) { super("body",owner); } }
  class HTMLHeadElement extends HTMLElement { constructor(owner=null) { super("head",owner); } }
  class HTMLHtmlElement extends HTMLElement { constructor(owner=null) { super("html",owner); } }
  class HTMLDivElement extends HTMLElement { constructor(owner=null) { super("div",owner); } }
  class HTMLSpanElement extends HTMLElement { constructor(owner=null) { super("span",owner); } }
  class HTMLStyleElement extends HTMLElement { constructor(owner=null) { super("style",owner); } }
  class SVGElement extends Element { constructor(tag,owner) { super(tag,owner,"http://www.w3.org/2000/svg"); } }
  for(const cls of [HTMLElement,HTMLScriptElement,HTMLIFrameElement,HTMLInputElement,HTMLAnchorElement])for(const name of ["name","type","nonce","integrity","crossOrigin","referrerPolicy","lang","dir"]){if(Object.getOwnPropertyDescriptor(cls.prototype,name))continue;Object.defineProperty(cls.prototype,name,{configurable:true,enumerable:true,get(){return this.getAttribute(name.toLowerCase())||"";},set(value){this.setAttribute(name.toLowerCase(),value);}});}
  for(const name of ["disabled","defer","noModule","required","readOnly","multiple","selected"]){Object.defineProperty(HTMLElement.prototype,name,{configurable:true,enumerable:true,get(){return this.hasAttribute(name.toLowerCase());},set(value){this.toggleAttribute(name.toLowerCase(),!!value);}});}
  const elementClasses={script:HTMLScriptElement,iframe:HTMLIFrameElement,a:HTMLAnchorElement,input:HTMLInputElement,body:HTMLBodyElement,head:HTMLHeadElement,html:HTMLHtmlElement,div:HTMLDivElement,span:HTMLSpanElement,style:HTMLStyleElement};
  class DocumentFragment extends Node {
    constructor(owner=null) { super(11,"#document-fragment",owner); }
    querySelector(selector) { return select(this,selector)[0]||null; } querySelectorAll(selector) { return new NodeList(select(this,selector)); }
    getElementById(id) { return descendants(this).find(n=>n.nodeType===1&&n.id===String(id))||null; }
  }
  class Document extends Node {
    constructor(url=__config.url) { super(9,"#document",null);state.get(this).url=String(url);state.get(this).readyState="loading";state.get(this).currentScript=null;this._activeElement=null; }
    get URL() { return state.get(this).url; } get documentURI() { return this.URL; } get location() { return realm.location; } get defaultView() { return realm; }
    get compatMode() { return this.doctype?"CSS1Compat":"BackCompat"; } get characterSet() { return "UTF-8"; } get charset() { return this.characterSet; } get contentType() { return "text/html"; }
    get documentElement() { return Array.from(this.children).find(n=>n.localName==="html")||this.firstElementChild; }
    get head() { return Array.from(this.documentElement?.children||[]).find(n=>n.localName==="head")||null; }
    get body() { return Array.from(this.documentElement?.children||[]).find(n=>n.localName==="body"||n.localName==="frameset")||null; }
    set body(value) { if(!value||!state.has(value)||value.nodeType!==1)fault("HierarchyRequestError","Body must be an element");if(this.body)this.documentElement.replaceChild(value,this.body);else this.documentElement.appendChild(value); }
    get doctype() { return Array.from(this.childNodes).find(n=>n.nodeType===10)||null; }
    get scripts() { return this.getElementsByTagName("script"); } get forms() { return this.getElementsByTagName("form"); } get images() { return this.getElementsByTagName("img"); } get links() { return new HTMLCollection(()=>descendants(this).filter(n=>n.nodeType===1&&["a","area"].includes(n.localName)&&n.hasAttribute("href"))); }
    get title() { return this.querySelector("title")?.textContent||""; } set title(value) { let title=this.querySelector("title");if(!title){title=this.createElement("title");this.head?.appendChild(title);}if(title)title.textContent=String(value); }
    get currentScript() { return state.get(this).currentScript; } set currentScript(value) { state.get(this).currentScript=value; }
    get readyState() { return state.get(this).readyState; } set readyState(value) { state.get(this).readyState=String(value); }
    get cookie() { return __host.cookieGet(); } set cookie(value) { __host.cookieSet(String(value)); }
    get activeElement() { return this._activeElement||this.body||this.documentElement; }
    createElement(tag) { tag=String(tag).toLowerCase();if(!/^[a-z][a-z0-9_.:-]*$/i.test(tag))fault("InvalidCharacterError","Invalid element name");const cls=elementClasses[tag];return cls?new cls(this):new HTMLElement(tag,this); }
    createElementNS(namespace,tag) { return namespace===XHTML?this.createElement(tag):namespace==="http://www.w3.org/2000/svg"?new SVGElement(String(tag),this):new Element(String(tag),this,namespace); }
    createTextNode(value) { return new Text(String(value),this); } createComment(value) { return new Comment(String(value),this); } createDocumentFragment() { return new DocumentFragment(this); } createAttribute(name) { const attr=new Attr(String(name).toLowerCase());state.get(attr).owner=this;return attr; }
    createEvent(name) { const types={Event,Events:Event,HTMLEvents:Event,MessageEvent,CustomEvent,MouseEvent,MouseEvents:MouseEvent,ErrorEvent,ProgressEvent};const ctor=types[String(name)];if(!ctor)fault("NotSupportedError","Unsupported event interface");return new ctor(""); }
    querySelector(selector) { return select(this,selector)[0]||null; } querySelectorAll(selector) { return new NodeList(select(this,selector)); }
    getElementById(id) { return descendants(this).find(n=>n.nodeType===1&&n.id===String(id))||null; }
    getElementsByTagName(tag) { return Element.prototype.getElementsByTagName.call(this,tag); } getElementsByClassName(names) { return Element.prototype.getElementsByClassName.call(this,names); } getElementsByName(name) { return new NodeList(()=>descendants(this).filter(n=>n.nodeType===1&&n.getAttribute("name")===String(name))); }
    importNode(node,deep=false) { const copy=cloneShallow(requireNode(node),this);if(deep)for(const child of node.childNodes)copy.appendChild(this.importNode(child,true));return copy; }
    adoptNode(node) { requireNode(node);if(node.nodeType===9)fault("NotSupportedError","Cannot adopt a Document");node.remove();adopt(node,this);return node; }
    open() { this.replaceChildren();state.get(this).writeBuffer="";state.get(this).writeOpen=true;this.readyState="loading";return this; }
    write(...values) { const s=state.get(this);if(!s.writeOpen)this.open();s.writeBuffer+=values.map(String).join("");this.replaceChildren();loadTree(this,__host.parseHTML(s.writeBuffer)); }
    writeln(...values) { this.write(...values,"\n"); }
    close() { const s=state.get(this);if(!s.writeOpen)return;s.writeOpen=false;microtask(()=>{this.readyState="interactive";trusted(this,new Event("DOMContentLoaded",{bubbles:true}));this.readyState="complete";trusted(realm,new Event("load"));}); }
  }
  class HTMLDocument extends Document {}
  function cloneShallow(node,document) { if(node.nodeType===1){const result=node.namespaceURI===XHTML?document.createElement(node.localName):document.createElementNS(node.namespaceURI,node.localName);for(const[name,value]of state.get(node).attributes)result.setAttribute(name,value);return result;}if(node.nodeType===3)return new Text(node.data,document);if(node.nodeType===8)return new Comment(node.data,document);if(node.nodeType===10)return new DocumentType(node.name,document);if(node.nodeType===11)return new DocumentFragment(document);if(node.nodeType===9)return new HTMLDocument(node.URL);fault("NotSupportedError","Unsupported clone node type"); }
  function nodeFromTree(tree,document) { let node;switch(Number(tree.nodeType)){case 1:node=document.createElement(tree.tag);for(const[name,value]of Object.entries(tree.attrs||{}))node.setAttribute(name,value);if(node instanceof HTMLScriptElement)state.get(node).forceAsync=false;break;case 3:node=document.createTextNode(tree.text||"");break;case 8:node=document.createComment(tree.text||"");break;case 10:node=new DocumentType(tree.tag||tree.text||"html",document);break;case 11:node=document.createDocumentFragment();break;default:return null;}for(const child of tree.children||[]){const parsed=nodeFromTree(child,document);if(parsed)node.appendChild(parsed);}return node; }
  function loadTree(document,tree) { const children=tree?.nodeType===9||tree?.nodeType===11?tree.children||[]:[tree];for(const child of children){const node=nodeFromTree(child,document);if(node)document.appendChild(node);} }
  function parseFragment(html,document,context) { const tree=__host.parseHTML(html,context||"body"),fragment=document.createDocumentFragment();if(tree.nodeType===9){const holder=new HTMLDocument(document.URL);loadTree(holder,tree);for(const child of [...Array.from(holder.head?.childNodes||[]),...Array.from(holder.body?.childNodes||[])])fragment.appendChild(child);return fragment;}for(const child of tree.children||[]){const node=nodeFromTree(child,document);if(node)fragment.appendChild(node);}return fragment; }
  const voidTags=new Set("area base br col embed hr img input link meta param source track wbr".split(" "));
  const escapeText=text=>String(text).replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;");
  function serialize(node,parentTag) { if(node.nodeType===3)return ["script","style"].includes(parentTag)?node.data:escapeText(node.data);if(node.nodeType===8)return "<!--"+node.data+"-->";if(node.nodeType===10)return "<!DOCTYPE "+node.name+">";if(node.nodeType!==1)return Array.from(node.childNodes).map(n=>serialize(n,"")).join("");const attrs=[...state.get(node).attributes].map(([name,value])=>" "+name+'="'+String(value).replace(/&/g,"&amp;").replace(/"/g,"&quot;")+'"').join("");return "<"+node.localName+attrs+">"+(voidTags.has(node.localName)?"":Array.from(node.childNodes).map(n=>serialize(n,node.localName)).join("")+"</"+node.localName+">"); }

  function selectorError() { fault("SyntaxError","Invalid or unsupported selector"); }
  function splitGroups(selector) { const groups=[];let start=0,depth=0,bracket=0,quote="",escaped=false;for(let i=0;i<selector.length;i++){const c=selector[i];if(escaped){escaped=false;continue;}if(c==="\\"){escaped=true;continue;}if(quote){if(c===quote)quote="";continue;}if(c==='"'||c==="'"){quote=c;continue;}if(c==="[")bracket++;if(c==="]"&&--bracket<0)selectorError();if(!bracket){if(c==="(")depth++;if(c===")"&&--depth<0)selectorError();if(c===","&&!depth){groups.push(selector.slice(start,i).trim());start=i+1;}}}if(quote||depth||bracket||escaped)selectorError();groups.push(selector.slice(start).trim());if(groups.some(g=>!g))selectorError();return groups; }
  function identifier(source,position) { let value="",i=position;while(i<source.length){const c=source[i];if(c==="\\"){i++;const hex=/^[0-9a-fA-F]{1,6}/.exec(source.slice(i));if(hex){const number=parseInt(hex[0],16);value+=String.fromCodePoint(number===0||number>0x10ffff?0xfffd:number);i+=hex[0].length;if(/\s/.test(source[i]||""))i++;}else {if(i>=source.length)selectorError();value+=source[i++];}}else if(/[\w-]/.test(c)||c.charCodeAt(0)>=128){value+=c;i++;}else break;}if(!value)selectorError();return [value,i]; }
  function balanced(source,start,open,close) { let depth=1,quote="",escaped=false;for(let i=start+1;i<source.length;i++){const c=source[i];if(escaped){escaped=false;continue;}if(c==="\\"){escaped=true;continue;}if(quote){if(c===quote)quote="";continue;}if(c==='"'||c==="'"){quote=c;continue;}if(c===open)depth++;if(c===close&&--depth===0)return [source.slice(start+1,i),i+1];}selectorError(); }
  function parseSelector(value) { return splitGroups(String(value)).map(source=>{const parts=[];let i=0,relation=null;while(i<source.length){while(/\s/.test(source[i]||""))i++;if(i===source.length)break;const compound={tag:null,ids:[],classes:[],attrs:[],pseudos:[]};let consumed=false;if(source[i]==="*"){compound.tag="*";i++;consumed=true;}else if(/[\w-]/.test(source[i])||source[i]==="\\"||source.charCodeAt(i)>=128){[compound.tag,i]=identifier(source,i);consumed=true;}while(i<source.length&&!/[\s>+~]/.test(source[i])){const c=source[i++];if(c==="#"||c==="."){let name;[name,i]=identifier(source,i);(c==="#"?compound.ids:compound.classes).push(name);}else if(c==="["){let text;[text,i]=balanced(source,i-1,"[","]");const match=/^\s*([\w:-]+)\s*(?:(~=|\|=|\^=|\$=|\*=|=)\s*(?:"([^"]*)"|'([^']*)'|([^\s]+))\s*([is])?\s*)?$/.exec(text);if(!match)selectorError();compound.attrs.push({name:match[1],op:match[2],value:match[3]??match[4]??match[5],insensitive:match[6]==="i"});}else if(c===":"){let name,arg=null;[name,i]=identifier(source,i);name=name.toLowerCase();if(source[i]==="(")[arg,i]=balanced(source,i,"(",")");const plain=["root","scope","first-child","last-child","only-child","first-of-type","last-of-type","only-of-type","empty","checked","disabled","enabled","link","any-link"];const functional=["not","is","where","nth-child","nth-last-child","nth-of-type","nth-last-of-type"];if(arg===null?!plain.includes(name):!functional.includes(name))selectorError();if(["not","is","where"].includes(name))arg=parseSelector(arg);else if(arg!==null)arg=nthExpression(arg);compound.pseudos.push({name,arg});}else selectorError();consumed=true;}if(!consumed)selectorError();parts.push({compound,relation});let whitespace=false;while(/\s/.test(source[i]||"")){whitespace=true;i++;}if(i===source.length)break;if(/[>+~]/.test(source[i])){relation=source[i++];while(/\s/.test(source[i]||""))i++;if(i===source.length)selectorError();}else if(whitespace)relation=" ";else selectorError();}if(!parts.length)selectorError();return parts;}); }
  function nthExpression(value) { value=String(value).replace(/\s/g,"").toLowerCase();if(value==="odd")return [2,1];if(value==="even")return [2,0];if(/^[+-]?\d+$/.test(value))return [0,Number(value)];const match=/^([+-]?\d*)n([+-]\d+)?$/.exec(value);if(!match)selectorError();return [match[1]===""||match[1]==="+"?1:match[1]==="-"?-1:Number(match[1]),Number(match[2]||0)]; }
  function matchesCompound(node,c,scope) {
    if(node.nodeType!==1)return c.tag===null&&!c.ids.length&&!c.classes.length&&!c.attrs.length&&c.pseudos.length>0&&c.pseudos.every(p=>p.name==="scope")&&node===scope;
    if(c.tag&&c.tag!=="*"&&node.localName.toLowerCase()!==c.tag.toLowerCase()||c.ids.some(id=>node.id!==id)||c.classes.some(name=>!node.classList.contains(name)))return false;
    for(const attr of c.attrs){let actual=node.getAttribute(attr.name),expected=attr.value;if(actual===null)return false;if(!attr.op)continue;if(attr.insensitive){actual=actual.toLowerCase();expected=expected.toLowerCase();}if(attr.op==="="?actual!==expected:attr.op==="~="?!actual.split(/\s+/).includes(expected):attr.op==="|="?actual!==expected&&!actual.startsWith(expected+"-"):attr.op==="^="?!expected||!actual.startsWith(expected):attr.op==="$="?!expected||!actual.endsWith(expected):!expected||!actual.includes(expected))return false;}
    for(const p of c.pseudos){const siblings=node.parentNode?Array.from(node.parentNode.children):[node],typed=siblings.filter(n=>n.localName===node.localName),index=siblings.indexOf(node)+1,typeIndex=typed.indexOf(node)+1;switch(p.name){case"scope":if(node!==scope)return false;break;case"root":if(node!==node.ownerDocument?.documentElement)return false;break;case"not":if(p.arg.some(g=>matchesComplex(node,g,scope)))return false;break;case"is":case"where":if(!p.arg.some(g=>matchesComplex(node,g,scope)))return false;break;case"empty":if(Array.from(node.childNodes).some(n=>n.nodeType===1||n.nodeType===3&&n.data.length))return false;break;case"first-child":if(index!==1)return false;break;case"last-child":if(index!==siblings.length)return false;break;case"only-child":if(siblings.length!==1)return false;break;case"first-of-type":if(typeIndex!==1)return false;break;case"last-of-type":if(typeIndex!==typed.length)return false;break;case"only-of-type":if(typed.length!==1)return false;break;case"checked":if(!["input","option"].includes(node.localName)||!(node.checked||node.selected))return false;break;case"disabled":case"enabled":if(!["input","button","select","textarea","option","optgroup","fieldset"].includes(node.localName)||(p.name==="disabled")!==node.disabled)return false;break;case"link":case"any-link":if(!["a","area","link"].includes(node.localName)||!node.hasAttribute("href"))return false;break;default:{let n=p.name.includes("of-type")?typeIndex:index;if(p.name.includes("last"))n=(p.name.includes("of-type")?typed.length:siblings.length)-n+1;const[a,b]=p.arg;if(a===0?n!==b:(n-b)/a<0||!Number.isInteger((n-b)/a))return false;}}}return true;
  }
  function matchesComplex(node,parts,scope,index=parts.length-1) { if(!node||!matchesCompound(node,parts[index].compound,scope))return false;if(index===0)return true;const relation=parts[index].relation;if(relation===">")return matchesComplex(node.parentNode,parts,scope,index-1);if(relation==="+")return matchesComplex(node.previousElementSibling,parts,scope,index-1);if(relation==="~"){for(let sibling=node.previousElementSibling;sibling;sibling=sibling.previousElementSibling)if(matchesComplex(sibling,parts,scope,index-1))return true;return false;}for(let ancestor=node.parentNode;ancestor;ancestor=ancestor.parentNode)if(matchesComplex(ancestor,parts,scope,index-1))return true;return false; }
  function select(root,selector) { const groups=parseSelector(selector),scope=root.nodeType===9?root.documentElement:root;return descendants(root).filter(node=>node.nodeType===1&&groups.some(group=>matchesComplex(node,group,scope))); }
  function trusted(target,event) { return dispatch(target,event,true); }
  const exports={DOMException,Event,MessageEvent,CustomEvent,ErrorEvent,ProgressEvent,MouseEvent,EventTarget,NodeList,HTMLCollection,MutationObserver,Node,CharacterData,Text,Comment,DocumentType,Attr,NamedNodeMap,DOMTokenList,CSSStyleDeclaration,Element,HTMLElement,HTMLScriptElement,HTMLIFrameElement,HTMLAnchorElement,HTMLInputElement,HTMLBodyElement,HTMLHeadElement,HTMLHtmlElement,HTMLDivElement,HTMLSpanElement,HTMLStyleElement,SVGElement,DocumentFragment,Document,HTMLDocument};
  for(const[name,value]of Object.entries(exports)){Object.defineProperty(value.prototype,Symbol.toStringTag,{value:name,configurable:true});Object.defineProperty(realm,name,{value,writable:true,configurable:true});}
  for(const name of ["addEventListener","removeEventListener","dispatchEvent"])Object.defineProperty(realm,name,{value:function(...args){return EventTarget.prototype[name].apply(realm,args);},configurable:true,writable:true});
  Object.defineProperties(realm,{__domDispatchTrusted:{value:trusted},__dispatchEvent:{value:(target,type)=>trusted(target,new Event(type))},__dispatchMessage:{value:(data,source,origin)=>trusted(realm,new MessageEvent("message",{data,source,origin}))},postMessage:{value:(data,targetOrigin="*",transfer)=>__host.postMessage(data,typeof targetOrigin==="object"?targetOrigin.targetOrigin||"/":targetOrigin),writable:true,configurable:true}});
  const document=new HTMLDocument(__config.url);Object.defineProperty(realm,"document",{value:document,writable:false,configurable:true});loadTree(document,__host.parseHTML(__config.html||"<!doctype html><html><head></head><body></body></html>"));
})();
