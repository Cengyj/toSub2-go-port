package jshost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestWorkerQueuedMessagesCanceledWithHost(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 80*time.Millisecond)
	defer cancel()
	input, _ := json.Marshal(map[string]any{"url": "https://fixture.test/", "loaderUrl": "https://fixture.test/loader.js", "loaderSource": `const worker=new Worker(URL.createObjectURL(new Blob(['while(true){}'])));for(let i=0;i<100;i++)worker.postMessage(i);while(true){}`})
	started := time.Now()
	err := Run(ctx, "sentinel", input, strings.NewReader(""), io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline, got %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("worker input backpressure prevented cancellation")
	}
}

func TestWorkerBackpressureDoesNotBlockCreatorTimer(t *testing.T) {
	value := runSDKFixture(t, `SentinelSDK={token:()=>new Promise(resolve=>{const worker=new Worker(URL.createObjectURL(new Blob(['while(true){}'])));for(let i=0;i<100;i++)worker.postMessage(i);setTimeout(()=>{worker.terminate();resolve('responsive');},30);})};`, nil)
	if value != "responsive" {
		t.Fatal(value)
	}
}

func TestZeroDelayTimersDoNotStarveTransportOrWorkers(t *testing.T) {
	value := runSDKFixture(t, `SentinelSDK={token:()=>new Promise(async(resolve,reject)=>{
      let active=true,ticks=0;function tick(){ticks++;if(active)setTimeout(tick,0);}setTimeout(tick,0);
      try{
        const response=await fetch('/while-timers-run');
        if(await response.text()!=='network-ready')throw Error('response');
        const worker=new Worker(URL.createObjectURL(new Blob(["postMessage('worker-ready');close();"])));
        worker.onmessage=e=>{active=false;resolve(ticks>0&&e.data==='worker-ready'?'fair':'failed');};
        worker.onerror=reject;
      }catch(e){active=false;reject(e);}
    })};`, func(request command) command {
		if stringField(request, "url") != "https://fixture.test/while-timers-run" {
			t.Fatal("unexpected request URL")
		}
		return command{"status": 200, "body": "network-ready"}
	})
	if value != "fair" {
		t.Fatal(value)
	}
}

func runSDKFixture(t *testing.T, source string, respond func(command) command) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	commandReader, commandWriter := io.Pipe()
	eventReader, eventWriter := io.Pipe()
	defer commandWriter.Close()
	defer eventReader.Close()
	input, _ := json.Marshal(map[string]any{"url": "https://fixture.test/page", "loaderUrl": "https://fixture.test/loader.js", "loaderSource": source})
	done := make(chan error, 1)
	go func() { defer eventWriter.Close(); done <- Run(ctx, "sentinel", input, commandReader, eventWriter) }()
	encoder := json.NewEncoder(commandWriter)
	if err := encoder.Encode(command{"type": "call", "method": "token", "flow": "fixture"}); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(eventReader)
	for {
		var event command
		if err := decoder.Decode(&event); err != nil {
			select {
			case runErr := <-done:
				t.Fatalf("runtime stopped: %v (%v)", runErr, err)
			default:
				t.Fatal(err)
			}
		}
		switch stringField(event, "type") {
		case "result":
			if err := encoder.Encode(command{"type": "shutdown"}); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			return stringField(event, "value")
		case "request":
			if respond == nil {
				t.Fatal("unexpected HTTP request")
			}
			request, ok := event["request"].(map[string]any)
			if !ok {
				t.Fatal("invalid request")
			}
			reply := respond(command(request))
			reply["id"] = request["id"]
			if err := encoder.Encode(reply); err != nil {
				t.Fatal(err)
			}
		case "failure":
			t.Fatalf("runtime failure: %v", event["code"])
		default:
			t.Fatalf("unexpected event type %v", event["type"])
		}
	}
}

func TestBlobWorkerBinaryTransferAndClone(t *testing.T) {
	value := runSDKFixture(t, `SentinelSDK={token:()=>new Promise((resolve,reject)=>{
      const url=URL.createObjectURL(new Blob(["onmessage=e=>{const value=e.data;value.bytes[0]++;postMessage({bytes:value.bytes,cycle:value.self===value,window:typeof window,name},[value.bytes.buffer]);};"],{type:'application/javascript'}));
      const worker=new Worker(url,{name:'isolated'});URL.revokeObjectURL(url);
      const bytes=new Uint8Array([3,255]),input={bytes};input.self=input;
      worker.onmessage=e=>{resolve(JSON.stringify({bytes:Array.from(e.data.bytes),cycle:e.data.cycle,window:e.data.window,name:e.data.name,detached:bytes.byteLength===0}));worker.terminate();};
      worker.onerror=e=>reject(new Error(e.message));worker.postMessage(input,[bytes.buffer]);
    })};`, nil)
	if value != `{"bytes":[4,255],"cycle":true,"window":"undefined","name":"isolated","detached":true}` {
		t.Fatal(value)
	}
}

func TestWorkerTerminateInterruptsIndependentEngine(t *testing.T) {
	started := time.Now()
	value := runSDKFixture(t, `SentinelSDK={token:()=>new Promise(resolve=>{const worker=new Worker(URL.createObjectURL(new Blob(['while(true){}'])));setTimeout(()=>{worker.terminate();resolve('terminated');},30);})};`, nil)
	if value != "terminated" {
		t.Fatal(value)
	}
	if time.Since(started) > time.Second {
		t.Fatal("worker prevented creator timer or termination")
	}
}

func TestWorkerScriptsAndFetchUseOwningTransport(t *testing.T) {
	var paths []string
	value := runSDKFixture(t, `SentinelSDK={token:()=>new Promise((resolve,reject)=>{const worker=new Worker('/worker.js');worker.onmessage=e=>{resolve(JSON.stringify(e.data));worker.terminate();};worker.onerror=e=>reject(new Error(e.message));worker.postMessage('start');})};`, func(request command) command {
		paths = append(paths, stringField(request, "url"))
		switch stringField(request, "url") {
		case "https://fixture.test/worker.js":
			return command{"status": 200, "body": `importScripts('/dependency.js');onmessage=async()=>{const r=await fetch('https://fixture.test/binary');postMessage({marker,bytes:Array.from(new Uint8Array(await r.arrayBuffer()))});};`}
		case "https://fixture.test/dependency.js":
			return command{"status": 200, "body": `const marker='imported';`}
		case "https://fixture.test/binary":
			if request["sourceUrl"] != "https://fixture.test/worker.js" {
				t.Fatal("worker request lost source context")
			}
			return command{"status": 200, "bodyBase64": "AID/"}
		default:
			t.Fatal("unexpected fixture route")
			return nil
		}
	})
	if value != `{"marker":"imported","bytes":[0,128,255]}` || len(paths) != 3 {
		t.Fatalf("value=%s, requests=%d", value, len(paths))
	}
}
