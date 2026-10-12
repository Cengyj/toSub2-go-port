(() => {
  'use strict';
  const host=globalThis.__storageHost;
  delete globalThis.__storageHost;
  const states=new WeakMap(),secret={};
  const domString=value=>{if(typeof value==='symbol')throw new TypeError('Cannot convert Symbol to DOMString');return String(value);};
  const encode=value=>{
    value=domString(value);
    if(!/[\ud800-\udfff]/.test(value))return 's:'+value;
    const units=new Uint16Array(value.length);
    for(let i=0;i<value.length;i++)units[i]=value.charCodeAt(i);
    return 'u:'+host.encode(units.buffer);
  };
  const decode=value=>{
    if(value===null)return null;
    if(value.startsWith('s:'))return value.slice(2);
    const units=new Uint16Array(host.decode(value.slice(2))),chunks=[];
    for(let i=0;i<units.length;i+=8192)chunks.push(String.fromCharCode(...units.subarray(i,i+8192)));
    return chunks.join('');
  };
  const kind=value=>{if(!states.has(value))throw new TypeError('Illegal invocation');return states.get(value);};
  const requireArguments=(length,count)=>{if(length<count)throw new TypeError('Missing Storage argument');};
  const read=(storage,key)=>decode(host.get(kind(storage),encode(key)));
  const write=(storage,key,value)=>{
    const area=kind(storage);key=domString(key);value=domString(value);
    if(!host.set(area,encode(key),encode(value),key.length+value.length))throw new DOMException('Storage quota exceeded','QuotaExceededError');
  };
  class Storage {
    constructor(token,area) {
      if(token!==secret)throw new TypeError('Illegal constructor');
      states.set(this,area);
      const proxy=new Proxy(this,{
        get(target,key,receiver){if(typeof key!=='string'||Reflect.has(target,key))return Reflect.get(target,key,receiver);const value=read(receiver,key);return value===null?undefined:value;},
        set(target,key,value,receiver){if(typeof key!=='string')return Reflect.set(target,key,value,receiver);write(receiver,key,value);return true;},
        deleteProperty(target,key){if(typeof key!=='string'||Reflect.has(target,key))return Reflect.deleteProperty(target,key);host.remove(kind(target),encode(key));return true;},
        has(target,key){return Reflect.has(target,key)||typeof key==='string'&&host.get(kind(target),encode(key))!==null;},
        ownKeys(target){return host.keys(kind(target)).map(decode).filter(key=>!Reflect.has(target,key)).concat(Reflect.ownKeys(target));},
        getOwnPropertyDescriptor(target,key){const own=Reflect.getOwnPropertyDescriptor(target,key);if(own||typeof key!=='string'||Reflect.has(target,key))return own;const value=read(target,key);return value===null?undefined:{value,writable:true,enumerable:true,configurable:true};},
        defineProperty(target,key,descriptor){if(typeof key!=='string')return Reflect.defineProperty(target,key,descriptor);if(!('value'in descriptor))return false;write(target,key,descriptor.value);return true;},
        preventExtensions(){return false;},
      });
      states.set(proxy,area);return proxy;
    }
    get length(){return host.length(kind(this));}
    key(index){requireArguments(arguments.length,1);const area=kind(this);index=Number(index)>>>0;const keys=host.keys(area);return keys[index]===undefined?null:decode(keys[index]);}
    getItem(key){requireArguments(arguments.length,1);return read(this,key);}
    setItem(key,value){requireArguments(arguments.length,2);write(this,key,value);}
    removeItem(key){requireArguments(arguments.length,1);host.remove(kind(this),encode(key));}
    clear(){host.clear(kind(this));}
  }
  Object.defineProperty(Storage.prototype,Symbol.toStringTag,{value:'Storage',configurable:true});
  for(const name of ['length','key','getItem','setItem','removeItem','clear'])Object.defineProperty(Storage.prototype,name,{...Object.getOwnPropertyDescriptor(Storage.prototype,name),enumerable:true});
  const local=new Storage(secret,'local'),session=new Storage(secret,'session');
  const expose=storage=>{if(host.opaque)throw new DOMException('Storage is unavailable for this origin','SecurityError');return storage;};
  Object.defineProperties(globalThis,{
    Storage:{value:Storage,writable:true,configurable:true},
    localStorage:{get:()=>expose(local),enumerable:true,configurable:true},
    sessionStorage:{get:()=>expose(session),enumerable:true,configurable:true},
  });
})();
