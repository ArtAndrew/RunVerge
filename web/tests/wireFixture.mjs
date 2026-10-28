// Current protocol header for ordinary JSON fixtures; negative compatibility tests use globalThis.Response.
export class Response extends globalThis.Response {
 constructor(body,init={}){const headers=new Headers(init.headers);if(!headers.has('X-Platform-API-Version'))headers.set('X-Platform-API-Version','0.14.0');super(body,{...init,headers})}
 static json(value,init={}){const headers=new Headers(init.headers);if(!headers.has('Content-Type'))headers.set('Content-Type','application/json');return new Response(JSON.stringify(value),{...init,headers})}
}
