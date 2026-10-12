(() => {
  'use strict';
  const workers = new WeakMap();
  URL.createObjectURL = blob => __host.blobURL(__webBlobBytes(blob).buffer, blob.type);
  URL.revokeObjectURL = url => __host.revokeBlobURL(String(url));
  const transfer = options => Array.isArray(options) ? options : options?.transfer || [];
  const validateTransfer = list => {
    const seen=new Set();
    for(const item of list){if(!(item instanceof ArrayBuffer)||seen.has(item))throw new DOMException('Invalid transfer','DataCloneError');new Uint8Array(item);seen.add(item);}
  };
  class Worker extends EventTarget {
    constructor(url, options = {}) {
      super();
      workers.set(this,__host.workerStart(this,new URL(String(url),location.href).href,options.type || 'classic',String(options.name || '')));
    }
    postMessage(value, options = []) {
      const list=transfer(options);validateTransfer(list);
      __host.workerPost(workers.get(this),value);
      for(const item of list)__host.detachBuffer(item);
    }
    terminate() {__host.workerStop(workers.get(this));workers.delete(this);}
  }
  Object.defineProperties(globalThis,{
    Worker:{value:Worker,writable:true,configurable:true},
    __workerMessage:{value:(target,data)=>__domDispatchTrusted(target,new MessageEvent('message',{data}))},
    __workerError:{value:(target,code)=>{const event=new Event('error',{cancelable:true});Object.defineProperty(event,'message',{value:code});if(__domDispatchTrusted(target,event))__host.failure(new Error(code));}},
  });
  if (__config.workerOrigin) {
    for (const key of ['window','document','parent','top','frames','frameElement','localStorage','sessionStorage','Storage']) delete globalThis[key];
    Object.defineProperty(globalThis,'name',{value:__config.workerName || '',configurable:true});
    globalThis.postMessage=(value,options=[])=>{const list=transfer(options);validateTransfer(list);__host.postMessage(value);for(const item of list)__host.detachBuffer(item);};
    globalThis.close=()=>__host.workerClose();
    globalThis.importScripts=(...urls)=>{for(const url of urls)__host.importScript(String(url));};
  }
})();
