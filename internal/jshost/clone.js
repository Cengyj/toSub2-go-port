(() => {
  'use strict';
  // Encode a graph, not JSON data: messages may contain cycles, shared buffers,
  // Maps and typed views. The wire representation never contains realm objects.
  const fail = () => {throw new DOMException('Value cannot be cloned', 'DataCloneError');};
  const pack = value => {
    const nodes = [], seen = new Map();
    const ref = value => {
      if (value === undefined) return {p:'undefined'};
      if (typeof value === 'bigint') return {p:'bigint',v:String(value)};
      if (typeof value === 'number' && (!Number.isFinite(value) || Object.is(value,-0))) return {p:'number',v:Object.is(value,-0)?'-0':String(value)};
      if (typeof value === 'function' || typeof value === 'symbol') return fail();
      if (value === null || typeof value !== 'object') return {v:value};
      if (seen.has(value)) return {r:seen.get(value)};
      const id = nodes.length, item = {}; seen.set(value,id); nodes.push(item);
      if (value instanceof ArrayBuffer) {item.t='buffer';item.v=Array.from(new Uint8Array(value));}
      else if (ArrayBuffer.isView(value)) {item.t='view';item.c=value.constructor.name;item.b=ref(value.buffer);item.o=value.byteOffset;item.l=value instanceof DataView?value.byteLength:value.length;}
      else if (value instanceof Map) {item.t='map';item.v=[...value].map(([k,v])=>[ref(k),ref(v)]);}
      else if (value instanceof Set) {item.t='set';item.v=[...value].map(ref);}
      else if (value instanceof Date) {item.t='date';item.v=ref(+value);}
      else if (value instanceof RegExp) {item.t='regexp';item.s=value.source;item.f=value.flags;}
      else if (value instanceof Blob) {item.t='blob';item.v=Array.from(__webBlobBytes(value));item.m=value.type;}
      else if (value instanceof Error) {item.t='error';item.n=value.name;item.m=value.message; if ('cause' in value) item.c=ref(value.cause);}
      else if (Array.isArray(value)) {item.t='array';item.l=value.length;item.v=Object.keys(value).map(k=>[k,ref(value[k])]);}
      else {
        if (value instanceof Node || value instanceof Promise || value instanceof WeakMap || value instanceof WeakSet || value===globalThis) return fail();
        item.t='object';item.v=Object.keys(value).map(k=>[k,ref(value[k])]);
      }
      return {r:id};
    };
    return {root:ref(value),nodes};
  };
  const unpack = graph => {
    const values = [], pending = new Set();
    const ref = record => {
      if ('r' in record) return node(record.r);
      if (record.p === 'undefined') return undefined;
      if (record.p === 'bigint') return BigInt(record.v);
      if (record.p === 'number') return Number(record.v);
      return record.v;
    };
    const node = id => {
      if (id in values) return values[id];
      if (pending.has(id)) return fail();
      pending.add(id);
      const item = graph.nodes[id];
      let value;
      switch (item.t) {
        case 'buffer': value=Uint8Array.from(item.v).buffer;break;
        case 'view': {
          const constructors={Int8Array,Uint8Array,Uint8ClampedArray,Int16Array,Uint16Array,Int32Array,Uint32Array,Float32Array,Float64Array,BigInt64Array,BigUint64Array,DataView};
          if (!constructors[item.c]) return fail();
          value=new constructors[item.c](ref(item.b),item.o,item.l);break;
        }
        case 'date':value=new Date(ref(item.v));break;
        case 'regexp':value=new RegExp(item.s,item.f);break;
        case 'blob':value=new Blob([Uint8Array.from(item.v)],{type:item.m});break;
        case 'map':value=new Map();break;
        case 'set':value=new Set();break;
        case 'array':value=new Array(item.l);break;
        case 'error':value=new Error(item.m);value.name=item.n;break;
        case 'object':value={};break;
        default:return fail();
      }
      values[id]=value;pending.delete(id);
      if (item.t==='map') for(const [k,v] of item.v)value.set(ref(k),ref(v));
      if (item.t==='set') for(const v of item.v)value.add(ref(v));
      if (item.t==='object'||item.t==='array') for(const [k,v] of item.v)Object.defineProperty(value,k,{value:ref(v),writable:true,configurable:true,enumerable:true});
      if (item.t==='error'&&item.c)value.cause=ref(item.c);
      return value;
    };
    return ref(graph.root);
  };
  Object.defineProperties(globalThis,{
    __clonePack:{value:pack},__cloneUnpack:{value:unpack},
    __clonePlain:{value:graph=>{const seen=new WeakSet();const text=JSON.stringify(unpack(graph),(_,value)=>{if(typeof value==='bigint')return String(value);if(value&&typeof value==='object'){if(seen.has(value))return null;seen.add(value);}return value;});return text===undefined?null:JSON.parse(text);}},
    structuredClone:{value:(value,options={})=>{
      const transfer=options.transfer||[];
      const unique=new Set();
      for (const item of transfer) {if (!(item instanceof ArrayBuffer)||unique.has(item)) return fail();unique.add(item);}
      const graph=pack(value),result=unpack(graph);
      for(const item of transfer)__host.detachBuffer(item);
      return result;
    },writable:true,configurable:true},
  });
})();
