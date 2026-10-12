(function (global, host) {
  "use strict";
  const tag = (prototype, name) => Object.defineProperty(prototype, Symbol.toStringTag, {value:name, configurable:true});
  const scalar = value => { let out=""; for (const ch of String(value)) { const n=ch.codePointAt(0); out+=n>=0xd800&&n<=0xdfff?"\ufffd":ch; } return out; };
  const error = (name, message) => { const e=new Error(message); e.name=name; return e; };
  const copyBytes = value => {
    if (value instanceof ArrayBuffer) return new Uint8Array(value.slice(0));
    if (ArrayBuffer.isView(value)) return new Uint8Array(value.buffer.slice(value.byteOffset,value.byteOffset+value.byteLength));
    throw new TypeError("Expected BufferSource");
  };

  class TextEncoder {
    get encoding() { return "utf-8"; }
    encode(value="") { return new Uint8Array(host.encodeText(scalar(value))); }
    encodeInto(value, destination) {
      if (!(destination instanceof Uint8Array)) throw new TypeError("Expected Uint8Array");
      let read=0,written=0;
      for (const ch of String(value)) {const bytes=this.encode(ch);if(written+bytes.length>destination.length)break;destination.set(bytes,written);written+=bytes.length;read+=ch.length;}
      return {read,written};
    }
  }
  tag(TextEncoder.prototype,"TextEncoder");
  const decoders=new WeakMap();
  class TextDecoder {
    constructor(label="utf-8",options={}) {
      const encoding=host.encoding(String(label).trim().toLowerCase());
      decoders.set(this,{encoding,fatal:!!options.fatal,ignoreBOM:!!options.ignoreBOM,started:false,pending:new Uint8Array(0)});
    }
    get encoding(){return decoders.get(this).encoding;}
    get fatal(){return decoders.get(this).fatal;}
    get ignoreBOM(){return decoders.get(this).ignoreBOM;}
    decode(value,options={}) {
      const state=decoders.get(this),input=value===undefined?new Uint8Array(0):copyBytes(value);
      const bytes=new Uint8Array(state.pending.length+input.length);bytes.set(state.pending);bytes.set(input,state.pending.length);
      const result=host.decodeText(bytes.buffer,state.encoding,state.fatal,!!options.stream);
      let text=result.text;
      if(!state.started&&text.length){state.started=true;if(!state.ignoreBOM&&text.charCodeAt(0)===0xfeff)text=text.slice(1);}
      state.pending=new Uint8Array(result.remaining);
      if(!options.stream){state.started=false;state.pending=new Uint8Array(0);}
      return text;
    }
  }
  tag(TextDecoder.prototype,"TextDecoder");

  function btoa(value) {
    value=String(value);const bytes=new Uint8Array(value.length);
    for(let i=0;i<value.length;i++){const n=value.charCodeAt(i);if(n>255)throw error("InvalidCharacterError","Value is not a Latin-1 byte string");bytes[i]=n;}
    return host.encodeBase64(bytes.buffer);
  }
  function atob(value) {
    let bytes;try{bytes=new Uint8Array(host.decodeBase64(String(value)));}catch(e){throw error("InvalidCharacterError","Invalid base64");}
    let out="";for(const byte of bytes)out+=String.fromCharCode(byte);return out;
  }

  const paramsState=new WeakMap();
  function parseParams(value) {
    const rows=[];value=String(value);if(value.startsWith("?"))value=value.slice(1);
    for(const pair of value.split("&")){if(!pair)continue;const at=pair.indexOf("=");rows.push([host.formDecode(at<0?pair:pair.slice(0,at)),host.formDecode(at<0?"":pair.slice(at+1))]);}
    return rows;
  }
  const changed = params => {const state=paramsState.get(params);if(state.update)state.update(params.toString());};
  class URLSearchParams {
    constructor(value="") {
      let rows=[];
      if(typeof value==="string")rows=parseParams(value);
      else if(value!=null&&typeof value[Symbol.iterator]==="function")for(const pair of value){const entries=Array.from(pair);if(entries.length!==2)throw new TypeError("Expected name/value pair");rows.push(entries.map(scalar));}
      else if(value!=null)for(const key of Object.keys(value))rows.push([scalar(key),scalar(value[key])]);
      paramsState.set(this,{rows,update:null});
    }
    get size(){return paramsState.get(this).rows.length;}
    append(name,value){paramsState.get(this).rows.push([scalar(name),scalar(value)]);changed(this);}
    delete(name,value){name=scalar(name);const matchValue=arguments.length>1;if(matchValue)value=scalar(value);const state=paramsState.get(this);state.rows=state.rows.filter(pair=>pair[0]!==name||matchValue&&pair[1]!==value);changed(this);}
    get(name){name=scalar(name);const pair=paramsState.get(this).rows.find(pair=>pair[0]===name);return pair?pair[1]:null;}
    getAll(name){name=scalar(name);return paramsState.get(this).rows.filter(pair=>pair[0]===name).map(pair=>pair[1]);}
    has(name,value){name=scalar(name);const matchValue=arguments.length>1;if(matchValue)value=scalar(value);return paramsState.get(this).rows.some(pair=>pair[0]===name&&(!matchValue||pair[1]===value));}
    set(name,value){name=scalar(name);value=scalar(value);const state=paramsState.get(this);let seen=false;state.rows=state.rows.filter(pair=>{if(pair[0]!==name)return true;if(seen)return false;seen=true;pair[1]=value;return true;});if(!seen)state.rows.push([name,value]);changed(this);}
    sort(){paramsState.get(this).rows.sort((a,b)=>a[0]<b[0]?-1:a[0]>b[0]?1:0);changed(this);}
    toString(){return paramsState.get(this).rows.map(pair=>host.formEncode(pair[0])+"="+host.formEncode(pair[1])).join("&");}
    *entries(){const rows=paramsState.get(this).rows;for(let i=0;i<rows.length;i++)yield rows[i].slice();}
    *keys(){for(const pair of this.entries())yield pair[0];}
    *values(){for(const pair of this.entries())yield pair[1];}
    forEach(callback,thisArg){for(const pair of this.entries())callback.call(thisArg,pair[1],pair[0],this);}
    [Symbol.iterator](){return this.entries();}
  }
  tag(URLSearchParams.prototype,"URLSearchParams");
  const urls=new WeakMap();
  function setURLParts(url,parts){const state=urls.get(url);state.parts=parts;paramsState.get(state.params).rows=parseParams(parts.search);}
  class URL {
    constructor(value,base) {
      const parts=host.resolveURL(scalar(value),base===undefined?undefined:scalar(base));
      const params=new URLSearchParams(parts.search);urls.set(this,{parts,params});
      paramsState.get(params).update=query=>{const state=urls.get(this);state.parts=host.setURL(state.parts.href,"search",query?"?"+query:"");};
    }
    get href(){return urls.get(this).parts.href;}
    set href(value){setURLParts(this,host.resolveURL(scalar(value),undefined));}
    get origin(){return urls.get(this).parts.origin;}
    get searchParams(){return urls.get(this).params;}
    toString(){return this.href;}
    toJSON(){return this.href;}
    static canParse(value,base){try{new URL(value,base);return true;}catch{return false;}}
    static parse(value,base){try{return new URL(value,base);}catch{return null;}}
  }
  for(const name of ["protocol","username","password","host","hostname","port","pathname","search","hash"]){Object.defineProperty(URL.prototype,name,{enumerable:true,configurable:true,get(){return urls.get(this).parts[name];},set(value){setURLParts(this,host.setURL(this.href,name,scalar(value)));}});}
  tag(URL.prototype,"URL");

  const blobs=new WeakMap();
  function bodyData(value,acceptStream=false) {
    if(value==null)return {bytes:new Uint8Array(0),type:"",present:false};
    if(acceptStream&&readableStates.has(value)){if(value.locked||readableStates.get(value).disturbed)throw bodyError();return {stream:value,type:"",present:true};}
    if(blobs.has(value)){const b=blobs.get(value);return {bytes:b.bytes.slice(),type:b.type,present:true};}
    if(value instanceof ArrayBuffer||ArrayBuffer.isView(value))return {bytes:copyBytes(value),type:"",present:true};
    if(value instanceof URLSearchParams)return {bytes:new TextEncoder().encode(value.toString()),type:"application/x-www-form-urlencoded;charset=UTF-8",present:true};
    return {bytes:new TextEncoder().encode(value),type:"text/plain;charset=UTF-8",present:true};
  }
  class Blob {
    constructor(parts=[],options={}) {
      if(parts==null||typeof parts[Symbol.iterator]!=="function")throw new TypeError("Blob parts must be iterable");
      const all=[];let length=0;
      for(let value of parts){if(typeof value==="string"&&options.endings==="native")value=value.replace(/\r\n|\r|\n/g,host.newline);const bytes=value==null?new TextEncoder().encode(String(value)):bodyData(value).bytes;all.push(bytes);length+=bytes.length;}
      const bytes=new Uint8Array(length);let offset=0;for(const part of all){bytes.set(part,offset);offset+=part.length;}
      let type=String(options.type??"");if(/[^\x20-\x7e]/.test(type))type="";blobs.set(this,{bytes,type:type.toLowerCase()});
    }
    get size(){return blobs.get(this).bytes.length;}
    get type(){return blobs.get(this).type;}
    slice(start=0,end=this.size,type="") {
      const size=this.size;start=Math.trunc(Number(start))||0;end=Math.trunc(Number(end))||0;
      start=start<0?Math.max(size+start,0):Math.min(start,size);end=end<0?Math.max(size+end,0):Math.min(end,size);
      return new Blob([blobs.get(this).bytes.slice(start,Math.max(start,end))],{type});
    }
    async arrayBuffer(){return blobs.get(this).bytes.slice().buffer;}
    async bytes(){return blobs.get(this).bytes.slice();}
    async text(){return new TextDecoder().decode(blobs.get(this).bytes);}
    stream(){return bytesStream(blobs.get(this).bytes);}
  }
  tag(Blob.prototype,"Blob");

  const headerState=new WeakMap();
  const headerName=value=>{value=String(value).toLowerCase();if(!/^[!#$%&'*+.^_`|~0-9a-z-]+$/.test(value))throw new TypeError("Invalid header name");return value;};
  const headerValue=value=>{value=String(value);if(/[^\x01-\xff]|[\r\n]/.test(value))throw new TypeError("Invalid header value");return value.replace(/^[\t ]+|[\t ]+$/g,"");};
  class Headers {
    constructor(init) {
      headerState.set(this,new Map());
      if(init instanceof Headers){for(const [name,values] of headerState.get(init))for(const value of values)this.append(name,value);}
      else if(init!=null&&typeof init[Symbol.iterator]==="function")for(const pair of init){const entries=Array.from(pair);if(entries.length!==2)throw new TypeError("Expected header pair");this.append(entries[0],entries[1]);}
      else if(init!=null)for(const name of Object.keys(init))this.append(name,init[name]);
    }
    append(name,value){name=headerName(name);value=headerValue(value);const state=headerState.get(this),values=state.get(name)||[];values.push(value);state.set(name,values);}
    set(name,value){headerState.get(this).set(headerName(name),[headerValue(value)]);}
    get(name){const values=headerState.get(this).get(headerName(name));return values?values.join(", "):null;}
    has(name){return headerState.get(this).has(headerName(name));}
    delete(name){headerState.get(this).delete(headerName(name));}
    getSetCookie(){return (headerState.get(this).get("set-cookie")||[]).slice();}
    *entries(){const state=headerState.get(this);for(const name of Array.from(state.keys()).sort()){const values=state.get(name);if(!values)continue;if(name==="set-cookie"){for(const value of values)yield [name,value];}else yield [name,values.join(", ")];}}
    *keys(){for(const pair of this.entries())yield pair[0];}
    *values(){for(const pair of this.entries())yield pair[1];}
    forEach(callback,thisArg){for(const pair of this.entries())callback.call(thisArg,pair[1],pair[0],this);}
    [Symbol.iterator](){return this.entries();}
  }
  tag(Headers.prototype,"Headers");

  const readableStates=new WeakMap(),readerStates=new WeakMap(),controllerStates=new WeakMap();
  function deferred() {
    let resolve,reject;
    const promise=new Promise((yes,no)=>{resolve=yes;reject=no;});
    return {promise,resolve,reject};
  }
  function streamState(stream) {
    const state=readableStates.get(stream);
    if(!state)throw new TypeError("Expected ReadableStream");
    return state;
  }
  function readerState(reader) {
    const state=readerStates.get(reader);
    if(!state)throw new TypeError("Expected ReadableStreamDefaultReader");
    return state;
  }
  function controllerState(controller) {
    const state=controllerStates.get(controller);
    if(!state)throw new TypeError("Expected ReadableStreamDefaultController");
    return state;
  }
  function closeStream(state) {
    if(state.status!=="readable")return;
    state.status="closed";
    if(state.reader){
      const reader=readerStates.get(state.reader);
      for(const request of reader.requests.splice(0))request.resolve({value:undefined,done:true});
      reader.closed.resolve();
    }
  }
  function errorStream(state,reason) {
    if(state.status!=="readable")return;
    state.status="errored";state.reason=reason;state.queue=[];state.queueSize=0;
    if(state.reader){
      const reader=readerStates.get(state.reader);
      for(const request of reader.requests.splice(0))request.reject(reason);
      reader.closed.reject(reason);
    }
  }
  function desiredSize(state) {
    return state.status==="errored"?null:state.status==="closed"?0:state.highWaterMark-state.queueSize;
  }
  function pullIfNeeded(state) {
    if(!state.started||state.status!=="readable"||state.closeRequested)return;
    const reader=state.reader&&readerStates.get(state.reader);
    if(!(reader&&reader.requests.length)&&desiredSize(state)<=0)return;
    if(state.pulling){state.pullAgain=true;return;}
    state.pulling=true;
    let result;
    try{result=state.pull&&state.pull.call(state.source,state.controller);}catch(reason){state.pulling=false;errorStream(state,reason);return;}
    Promise.resolve(result).then(()=>{
      state.pulling=false;
      if(state.pullAgain){state.pullAgain=false;pullIfNeeded(state);}
    },reason=>{state.pulling=false;errorStream(state,reason);});
  }
  function cancelStream(state,reason) {
    state.disturbed=true;
    if(state.status==="closed")return Promise.resolve();
    if(state.status==="errored")return Promise.reject(state.reason);
    state.queue=[];state.queueSize=0;closeStream(state);
    try{return Promise.resolve(state.cancel&&state.cancel.call(state.source,reason)).then(()=>undefined);}catch(reason){return Promise.reject(reason);}
  }
  const controllerToken={};
  class ReadableStreamDefaultController {
    constructor(token,state){if(token!==controllerToken)throw new TypeError("Illegal constructor");controllerStates.set(this,state);}
    get desiredSize(){return desiredSize(controllerState(this));}
    close(){const state=controllerState(this);if(state.status!=="readable"||state.closeRequested)throw new TypeError("Stream cannot be closed");state.closeRequested=true;if(!state.queue.length)closeStream(state);}
    enqueue(chunk){
      const state=controllerState(this);
      if(state.status!=="readable"||state.closeRequested)throw new TypeError("Stream cannot accept a chunk");
      const reader=state.reader&&readerStates.get(state.reader);
      if(reader&&reader.requests.length)reader.requests.shift().resolve({value:chunk,done:false});
      else{
        let size;
        try{size=Number(state.size?state.size.call(undefined,chunk):1);if(!Number.isFinite(size)||size<0)throw new RangeError("Invalid chunk size");}
        catch(reason){errorStream(state,reason);throw reason;}
        state.queue.push({value:chunk,size});state.queueSize+=size;
      }
      pullIfNeeded(state);
    }
    error(reason){errorStream(controllerState(this),reason);}
  }
  tag(ReadableStreamDefaultController.prototype,"ReadableStreamDefaultController");
  class ReadableStreamDefaultReader {
    constructor(stream){
      const state=streamState(stream);
      if(state.reader)throw new TypeError("Stream is locked");
      const closed=deferred();closed.promise.catch(()=>{});
      readerStates.set(this,{stream,state,closed,requests:[]});state.reader=this;
      if(state.status==="closed")closed.resolve();else if(state.status==="errored")closed.reject(state.reason);
    }
    get closed(){return readerState(this).closed.promise;}
    read(){
      let reader;try{reader=readerState(this);}catch(reason){return Promise.reject(reason);}
      if(!reader.stream)return Promise.reject(new TypeError("Reader lock released"));
      const state=reader.state;state.disturbed=true;
      if(state.status==="closed")return Promise.resolve({value:undefined,done:true});
      if(state.status==="errored")return Promise.reject(state.reason);
      if(state.queue.length){
        const chunk=state.queue.shift();state.queueSize=Math.max(0,state.queueSize-chunk.size);
        if(state.closeRequested&&!state.queue.length)closeStream(state);else pullIfNeeded(state);
        return Promise.resolve({value:chunk.value,done:false});
      }
      const request=deferred();reader.requests.push(request);pullIfNeeded(state);return request.promise;
    }
    cancel(reason){
      let reader;try{reader=readerState(this);}catch(reason){return Promise.reject(reason);}
      return reader.stream?cancelStream(reader.state,reason):Promise.reject(new TypeError("Reader lock released"));
    }
    releaseLock(){
      const reader=readerState(this);if(!reader.stream)return;
      const reason=new TypeError("Reader lock released");
      for(const request of reader.requests.splice(0))request.reject(reason);
      if(reader.state.status!=="readable"){reader.closed=deferred();reader.closed.promise.catch(()=>{});}
      reader.closed.reject(reason);reader.state.reader=null;reader.stream=null;
    }
  }
  tag(ReadableStreamDefaultReader.prototype,"ReadableStreamDefaultReader");
  class ReadableStream {
    constructor(source={},strategy={}){
      if(source===null||typeof source!=="object"&&typeof source!=="function")throw new TypeError("Invalid underlying source");
      if(source.type!==undefined){if(String(source.type)==="bytes")throw error("NotSupportedError","Byte streams and BYOB readers are not supported");throw new RangeError("Invalid stream type");}
      const start=source.start,pull=source.pull,cancel=source.cancel,size=strategy.size;
      for(const callback of [start,pull,cancel,size])if(callback!==undefined&&typeof callback!=="function")throw new TypeError("Expected stream callback");
      const highWaterMark=Number(strategy.highWaterMark??1);
      if(Number.isNaN(highWaterMark)||highWaterMark<0)throw new RangeError("Invalid high water mark");
      const state={source,pull,cancel,size,highWaterMark,queue:[],queueSize:0,status:"readable",reason:undefined,reader:null,disturbed:false,started:false,pulling:false,pullAgain:false,closeRequested:false};
      readableStates.set(this,state);state.controller=new ReadableStreamDefaultController(controllerToken,state);
      const result=start&&start.call(source,state.controller);
      Promise.resolve(result).then(()=>{state.started=true;pullIfNeeded(state);},reason=>errorStream(state,reason));
    }
    get locked(){return !!streamState(this).reader;}
    getReader(options={}){streamState(this);if(options.mode!==undefined){if(String(options.mode)==="byob")throw error("NotSupportedError","BYOB readers are not supported");throw new TypeError("Invalid reader mode");}return new ReadableStreamDefaultReader(this);}
    cancel(reason){let state;try{state=streamState(this);}catch(reason){return Promise.reject(reason);}return state.reader?Promise.reject(new TypeError("Stream is locked")):cancelStream(state,reason);}
    tee(){
      streamState(this);
      const reader=this.getReader(),canceled=[false,false],reasons=[],controllers=[];
      const cancellation=deferred();let reading=null,finished=false;
      const finish=()=>{if(finished)return;finished=true;cancellation.resolve();};
      const fail=reason=>{if(finished)return;for(let i=0;i<2;i++)if(!canceled[i])controllers[i].error(reason);finish();};
      const pull=()=>{
        if(finished)return;
        if(reading)return reading;
        reading=reader.read().then(result=>{
          reading=null;if(finished)return;
          if(result.done){for(let i=0;i<2;i++)if(!canceled[i])controllers[i].close();finish();}
          else for(let i=0;i<2;i++)if(!canceled[i])controllers[i].enqueue(result.value);
        },reason=>{reading=null;fail(reason);});
        return reading;
      };
      const branches=[0,1].map(index=>new ReadableStream({
        start(controller){controllers[index]=controller;},pull,
        cancel(reason){
          canceled[index]=true;reasons[index]=reason;
          if(canceled[0]&&canceled[1]&&!finished){finished=true;reader.cancel(reasons).then(cancellation.resolve,cancellation.reject);}
          return cancellation.promise;
        }
      }));
      reader.closed.catch(fail);
      return branches;
    }
    pipeTo(destination,options={}){
      try{streamState(this);options=pipeOptions(options);if(this.locked||destination?.locked)throw new TypeError("Stream is locked");if(typeof destination?.getWriter!=="function")throw new TypeError("Expected writable stream");}
      catch(reason){return Promise.reject(reason);}
      return pipeStream(this,destination,options);
    }
    pipeThrough(transform,options={}){
      streamState(this);options=pipeOptions(options);if(!readableStates.has(transform?.readable)||typeof transform?.writable?.getWriter!=="function")throw new TypeError("Expected readable/writable transform");
      if(this.locked||transform.writable.locked)throw new TypeError("Stream is locked");
      this.pipeTo(transform.writable,options).catch(()=>{});return transform.readable;
    }
    values(options={}){
      const reader=this.getReader();let done=false,ongoing=Promise.resolve();
      const sequence=callback=>{const next=ongoing.then(callback);ongoing=next.catch(()=>{});return next;};
      const iterator={
        next(){return sequence(async()=>{if(done)return {value:undefined,done:true};try{const result=await reader.read();if(result.done){done=true;reader.releaseLock();}return result;}catch(reason){done=true;reader.releaseLock();throw reason;}});},
        return(value){return sequence(async()=>{if(!done){done=true;try{if(!options.preventCancel)await reader.cancel(value);}finally{reader.releaseLock();}}return {value,done:true};});},
        [Symbol.asyncIterator](){return this;}
      };
      return iterator;
    }
    [Symbol.asyncIterator](){return this.values();}
  }
  tag(ReadableStream.prototype,"ReadableStream");
  function pipeOptions(options){
    options=options??{};
    const parsed={preventClose:!!options.preventClose,preventAbort:!!options.preventAbort,preventCancel:!!options.preventCancel,signal:options.signal};
    const signal=parsed.signal;
    if(signal!==undefined&&(!signal||typeof signal.addEventListener!=="function"||typeof signal.removeEventListener!=="function"))throw new TypeError("Expected AbortSignal");
    return parsed;
  }
  async function pipeStream(stream,destination,options){
    const reader=stream.getReader();let writer;
    try{writer=destination.getWriter();for(const method of ["write","close","abort","releaseLock"])if(typeof writer?.[method]!=="function")throw new TypeError("Invalid writable stream writer");}
    catch(reason){reader.releaseLock();if(typeof writer?.releaseLock==="function")writer.releaseLock();throw reason;}
    const signal=options.signal,terminal=deferred(),stopToken={};let stopping=false,shutdown=null,abortListener;
    function stop(kind,reason){
      if(stopping)return shutdown;
      stopping=true;terminal.resolve();
      shutdown=(async()=>{
        const actions=[];
        if(kind==="sourceClose"){if(!options.preventClose)actions.push(Promise.resolve().then(()=>writer.close()));}
        else{
          if((kind==="sourceError"||kind==="abort")&&!options.preventAbort)actions.push(Promise.resolve().then(()=>writer.abort(reason)));
          if((kind==="destinationError"||kind==="abort")&&!options.preventCancel)actions.push(Promise.resolve().then(()=>reader.cancel(reason)));
        }
        try{await Promise.all(actions);}catch(actionError){return {failed:true,reason:actionError};}
        return {failed:kind!=="sourceClose",reason};
      })();
      return shutdown;
    }
    async function operation(callback,kind){
      try{return await Promise.race([Promise.resolve().then(()=>{if(stopping)throw stopToken;return callback();}),terminal.promise.then(()=>{throw stopToken;})]);}
      catch(reason){if(reason!==stopToken)stop(kind,reason);throw reason;}
    }
    try{
      if(signal!==undefined){
        abortListener=()=>stop("abort",signal.reason===undefined?error("AbortError","The operation was aborted"):signal.reason);
        if(signal.aborted)abortListener();else signal.addEventListener("abort",abortListener,{once:true});
      }
      reader.closed.catch(reason=>stop("sourceError",reason));
      if(writer.closed!==undefined)Promise.resolve(writer.closed).then(()=>{if(!stopping)stop("destinationError",new TypeError("Destination closed before the source completed"));},reason=>stop("destinationError",reason));
      while(!stopping){
        await operation(()=>writer.ready,"destinationError");
        const result=await operation(()=>reader.read(),"sourceError");
        if(result.done){stop("sourceClose");break;}
        await operation(()=>writer.write(result.value),"destinationError");
      }
    }catch(reason){if(reason!==stopToken&&!stopping)stop("destinationError",reason);}
    try{const result=await shutdown;if(result.failed)throw result.reason;}
    finally{if(abortListener)signal.removeEventListener("abort",abortListener);reader.releaseLock();writer.releaseLock();}
  }
  class CountQueuingStrategy {
    constructor(options){this.highWaterMark=Number(options.highWaterMark);}
    size(){return 1;}
  }
  class ByteLengthQueuingStrategy {
    constructor(options){this.highWaterMark=Number(options.highWaterMark);}
    size(chunk){return chunk.byteLength;}
  }
  tag(CountQueuingStrategy.prototype,"CountQueuingStrategy");tag(ByteLengthQueuingStrategy.prototype,"ByteLengthQueuingStrategy");
  function bytesStream(bytes){const owned=bytes.slice();return new ReadableStream({start(controller){if(owned.length)controller.enqueue(owned);controller.close();}});}
  const bodies=new WeakMap();
  function bodyError(){return new TypeError("Body is already used or locked");}
  function bodyStream(state){return state.stream||(state.stream=bytesStream(state.bytes));}
  function bodyUnusable(state){return state.stream&&(state.stream.locked||streamState(state.stream).disturbed);}
  async function consume(state){
    if(bodyUnusable(state))throw bodyError();
    if(!state.present)return new Uint8Array(0);
    const reader=bodyStream(state).getReader(),chunks=[];let length=0;
    try{
      while(true){const result=await reader.read();if(result.done)break;if(!(result.value instanceof Uint8Array))throw new TypeError("Body stream chunks must be Uint8Array");const chunk=result.value.slice();chunks.push(chunk);length+=chunk.length;}
    }catch(reason){try{await reader.cancel(reason);}catch{}throw reason;}
    finally{reader.releaseLock();}
    const bytes=new Uint8Array(length);let offset=0;for(const chunk of chunks){bytes.set(chunk,offset);offset+=chunk.length;}return bytes;
  }
  function cloneBodyData(state){
    if(bodyUnusable(state))throw bodyError();
    if(!state.stream)return {bytes:state.bytes.slice(),present:state.present,type:""};
    const branches=state.stream.tee();state.stream=branches[0];return {stream:branches[1],present:state.present,type:""};
  }
  function transferBodyData(state){
    if(bodyUnusable(state))throw bodyError();
    if(!state.present)return {bytes:new Uint8Array(0),present:false,type:""};
    const original=bodyStream(state),reader=original.getReader();streamState(original).disturbed=true;
    const stream=new ReadableStream({
      pull(controller){return reader.read().then(result=>{if(result.done)controller.close();else controller.enqueue(result.value);});},
      cancel(reason){return reader.cancel(reason);}
    });
    return {stream,present:true,type:""};
  }
  class Body {
    get bodyUsed(){const state=bodies.get(this);return !!(state.stream&&streamState(state.stream).disturbed);}
    get body(){const state=bodies.get(this);return state.present?bodyStream(state):null;}
    async arrayBuffer(){return (await consume(bodies.get(this))).buffer;}
    async bytes(){return consume(bodies.get(this));}
    async text(){return new TextDecoder().decode(await consume(bodies.get(this)));}
    async json(){return JSON.parse(await this.text());}
    async blob(){return new Blob([await consume(bodies.get(this))],{type:this.headers.get("content-type")||""});}
  }
  const attachBody=(object,data)=>bodies.set(object,{bytes:data.bytes??null,present:data.present,stream:data.stream||null});
  function initHeaders(body,init){const headers=new Headers(init);if(body.type&&!headers.has("content-type"))headers.set("content-type",body.type);return headers;}
  class Request extends Body {
    constructor(input,init={}) {
      super();const prior=input instanceof Request?input:null;
      this.url=new URL(prior?prior.url:String(input),global.location?.href).href;
      let method=String(init.method??prior?.method??"GET");if(!/^[!#$%&'*+.^_`|~0-9a-zA-Z-]+$/.test(method))throw new TypeError("Invalid method");const upper=method.toUpperCase();if(["CONNECT","TRACE","TRACK"].includes(upper))throw new TypeError("Forbidden method");if(["DELETE","GET","HEAD","OPTIONS","POST","PUT"].includes(upper))method=upper;this.method=method;
      if((method==="GET"||method==="HEAD")&&(init.body!=null||prior&&bodies.get(prior).present))throw new TypeError("GET/HEAD cannot have a body");
      let data=init.body!=null?bodyData(init.body,true):prior?{present:bodies.get(prior).present,type:""}:bodyData(null);
      if((method==="GET"||method==="HEAD")&&data.present)throw new TypeError("GET/HEAD cannot have a body");
      this.headers=initHeaders(data,init.headers??prior?.headers);
      for(const [key,value] of Object.entries({mode:"cors",credentials:"same-origin",cache:"default",redirect:"follow",referrer:"about:client",referrerPolicy:"",integrity:"",keepalive:false,signal:null})){this[key]=init[key]??prior?.[key]??value;}
      if(prior&&init.body==null)data=transferBodyData(bodies.get(prior));attachBody(this,data);
    }
    clone(){const data=cloneBodyData(bodies.get(this)),init={method:this.method,headers:this.headers,body:data.present?(data.stream||data.bytes):null};for(const key of ["mode","credentials","cache","redirect","referrer","referrerPolicy","integrity","keepalive","signal"])init[key]=this[key];return new Request(this.url,init);}
  }
  tag(Request.prototype,"Request");
  class Response extends Body {
    constructor(body=null,init={}) {
      super();const status=Number(init.status??200);if(!Number.isInteger(status)||status<200||status>599)throw new RangeError("Invalid response status");
      const statusText=String(init.statusText??"");if(/[^\x09\x20-\x7e\x80-\xff]/.test(statusText))throw new TypeError("Invalid status text");
      const data=bodyData(body,true);if([204,205,304].includes(status)&&data.present)throw new TypeError("Null-body status cannot have a body");
      this.status=status;this.statusText=statusText;this.headers=initHeaders(data,init.headers);this.url="";this.redirected=false;this.type="default";attachBody(this,data);
    }
    get ok(){return this.status>=200&&this.status<=299;}
    clone(){const state=bodies.get(this),data=cloneBodyData(state);const copy=new Response(data.present?(data.stream||data.bytes):null,{status:this.status||200,statusText:this.statusText,headers:this.headers});copy.url=this.url;copy.redirected=this.redirected;copy.type=this.type;copy.status=this.status;return copy;}
    static error(){const response=new Response();response.status=0;response.type="error";attachBody(response,bodyData(null));return response;}
    static redirect(url,status=302){if(![301,302,303,307,308].includes(Number(status)))throw new RangeError("Invalid redirect status");return new Response(null,{status,headers:{location:new URL(url,global.location?.href).href}});}
    static json(value,init={}){const body=JSON.stringify(value);if(body===undefined)throw new TypeError("Not JSON serializable");const headers=new Headers(init.headers);if(!headers.has("content-type"))headers.set("content-type","application/json");return new Response(body,{...init,headers});}
  }
  tag(Response.prototype,"Response");

  const crypto={
    getRandomValues(view){if(!ArrayBuffer.isView(view)||view instanceof DataView||view instanceof Float32Array||view instanceof Float64Array)throw error("TypeMismatchError","Expected integer typed array");if(view.byteLength>65536)throw error("QuotaExceededError","Random byte quota exceeded");new Uint8Array(view.buffer,view.byteOffset,view.byteLength).set(new Uint8Array(host.randomBytes(view.byteLength)));return view;},
    randomUUID(){return host.randomUUID();},
    subtle:{digest(algorithm,data){try{const name=typeof algorithm==="string"?algorithm:String(algorithm.name);if(!["SHA-1","SHA-256","SHA-384","SHA-512"].includes(name.toUpperCase()))return Promise.reject(error("NotSupportedError","Unsupported digest algorithm"));return Promise.resolve(host.digest(name,copyBytes(data).buffer));}catch(e){return Promise.reject(e);}}}
  };
  tag(crypto,"Crypto");tag(crypto.subtle,"SubtleCrypto");

  const configuration=typeof __config==="object"?__config:{};
  const epoch=Date.now();
  const clock=()=>typeof __host==="object"&&typeof __host.now==="function"?__host.now():Date.now()-epoch;
  class PerformanceEntry {
    constructor(init={}){Object.assign(this,{name:"",entryType:"",startTime:0,duration:0},init);}
    toJSON(){return {...this};}
  }
  class PerformanceResourceTiming extends PerformanceEntry {
    constructor(init={}){super({initiatorType:"",nextHopProtocol:"",workerStart:0,redirectStart:0,redirectEnd:0,fetchStart:0,domainLookupStart:0,domainLookupEnd:0,connectStart:0,secureConnectionStart:0,connectEnd:0,requestStart:0,responseStart:0,responseEnd:0,transferSize:0,encodedBodySize:0,decodedBodySize:0,...init});}
  }
  class PerformanceNavigationTiming extends PerformanceResourceTiming {
    constructor(init={}){super({type:"navigate",redirectCount:0,unloadEventStart:0,unloadEventEnd:0,domInteractive:0,domContentLoadedEventStart:0,domContentLoadedEventEnd:0,domComplete:0,loadEventStart:0,loadEventEnd:0,...init});}
  }
  tag(PerformanceEntry.prototype,"PerformanceEntry");tag(PerformanceResourceTiming.prototype,"PerformanceResourceTiming");tag(PerformanceNavigationTiming.prototype,"PerformanceNavigationTiming");
  const performanceEntries=[],observers=new Set();
  const entryFrom=init=>init.entryType==="navigation"?new PerformanceNavigationTiming(init):init.entryType==="resource"?new PerformanceResourceTiming(init):new PerformanceEntry(init);
  const list=entries=>({getEntries:()=>entries.slice(),getEntriesByType:type=>entries.filter(entry=>entry.entryType===String(type)),getEntriesByName:(name,type)=>entries.filter(entry=>entry.name===String(name)&&(type===undefined||entry.entryType===String(type)))});
  function notify(observer,entries){const state=observer._state;if(!state.active)return;state.pending.push(...entries.filter(entry=>state.types.includes(entry.entryType)));if(!state.pending.length||state.queued)return;state.queued=true;Promise.resolve().then(()=>{state.queued=false;if(!state.active||!state.pending.length)return;const pending=state.pending.splice(0);state.callback(list(pending),observer);});}
  class PerformanceObserver {
    constructor(callback){if(typeof callback!=="function")throw new TypeError("Expected observer callback");Object.defineProperty(this,"_state",{value:{callback,types:[],active:false,pending:[],queued:false}});}
    observe(options={}){const state=this._state;if(options.type!==undefined&&options.entryTypes!==undefined)throw new TypeError("Incompatible observer options");state.types=options.type!==undefined?[String(options.type)]:Array.from(options.entryTypes||[],String);state.active=true;observers.add(this);if(options.buffered)notify(this,performanceEntries);}
    disconnect(){this._state.active=false;this._state.pending=[];observers.delete(this);}
    takeRecords(){return this._state.pending.splice(0);}
    static get supportedEntryTypes(){return ["mark","measure","navigation","resource"];}
  }
  tag(PerformanceObserver.prototype,"PerformanceObserver");
  const addEntry=init=>{const entry=entryFrom(init);performanceEntries.push(entry);for(const observer of observers)notify(observer,[entry]);return entry;};
  class Performance {
    constructor(){this.timeOrigin=Number(configuration.timeOrigin??Date.now()-clock());const provided=configuration.performanceMemory||configuration.memory||{};this.memory={jsHeapSizeLimit:Number(provided.jsHeapSizeLimit??configuration.jsHeapSizeLimit??4395630592),totalJSHeapSize:Number(provided.totalJSHeapSize??configuration.totalJSHeapSize??0),usedJSHeapSize:Number(provided.usedJSHeapSize??configuration.usedJSHeapSize??0)};}
    now(){return clock();}
    getEntries(){return performanceEntries.slice();}
    getEntriesByType(type){return list(performanceEntries).getEntriesByType(type);}
    getEntriesByName(name,type){return list(performanceEntries).getEntriesByName(name,type);}
    clearResourceTimings(){for(let i=performanceEntries.length-1;i>=0;i--)if(performanceEntries[i].entryType==="resource")performanceEntries.splice(i,1);}
    mark(name,options={}){return addEntry({name:String(name),entryType:"mark",startTime:Number(options.startTime??clock()),duration:0,detail:options.detail??null});}
    clearMarks(name){for(let i=performanceEntries.length-1;i>=0;i--)if(performanceEntries[i].entryType==="mark"&&(name===undefined||performanceEntries[i].name===String(name)))performanceEntries.splice(i,1);}
    measure(name,start,end){const markTime=value=>typeof value==="number"?value:value===undefined?0:(performanceEntries.filter(entry=>entry.entryType==="mark"&&entry.name===String(value)).pop()||(()=>{throw error("SyntaxError","Unknown performance mark");})()).startTime;const a=markTime(start),b=end===undefined?clock():markTime(end);return addEntry({name:String(name),entryType:"measure",startTime:a,duration:b-a});}
    clearMeasures(name){for(let i=performanceEntries.length-1;i>=0;i--)if(performanceEntries[i].entryType==="measure"&&(name===undefined||performanceEntries[i].name===String(name)))performanceEntries.splice(i,1);}
    toJSON(){return {timeOrigin:this.timeOrigin};}
  }
  tag(Performance.prototype,"Performance");
  const performance=new Performance();for(const entry of Array.from(configuration.performanceEntries||[]))addEntry({...entry});
  Object.defineProperty(performance,"__addEntry",{value:addEntry,enumerable:false});
  for(const [name,value] of Object.entries({TextEncoder,TextDecoder,atob,btoa,URL,URLSearchParams,Blob,Headers,Request,Response,ReadableStream,ReadableStreamDefaultReader,ReadableStreamDefaultController,CountQueuingStrategy,ByteLengthQueuingStrategy,crypto,PerformanceEntry,PerformanceResourceTiming,PerformanceNavigationTiming,PerformanceObserver,Performance,performance})){Object.defineProperty(global,name,{value,writable:true,configurable:true,enumerable:false});}
  Object.defineProperty(global,"__webBlobBytes",{value:blob=>{if(!blobs.has(blob))throw new TypeError("Expected Blob");return blobs.get(blob).bytes.slice();},configurable:true,enumerable:false});
})(globalThis, __webPlatformBridge);
