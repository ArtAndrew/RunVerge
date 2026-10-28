# SPDX-License-Identifier: Apache-2.0
"""Independent MCP wire client for a real TLS product endpoint; no database API."""
import json
import os
from pathlib import Path
import ssl
import sys
from urllib.error import HTTPError
from urllib.request import Request, build_opener, HTTPSHandler, ProxyHandler, HTTPRedirectHandler


class NoRedirects(HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs): return None


def main():
    origin, credential_path, ca_path = sys.argv[1:]
    assert os.environ["DATABASE_URL"].startswith("postgres://invalid:")
    credentials=json.loads(Path(credential_path).read_text())
    opener=build_opener(ProxyHandler({}),NoRedirects(),HTTPSHandler(context=ssl.create_default_context(cafile=ca_path)))
    results=[]
    try:
        for identifier,method,params in [(1,"server/discover",{}),(2,"tools/list",{}),(3,"tools/call",{"name":"platform_capabilities","arguments":{}})]:
            params["_meta"]={"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}
            headers={"Authorization":"Bearer "+credentials["token"],"X-Organization-ID":credentials["organization_id"],"Content-Type":"application/json","Accept":"application/json, text/event-stream","MCP-Protocol-Version":"2026-07-28","MCP-Method":method}
            if "name" in params:headers["MCP-Name"]=params["name"]
            body=json.dumps({"jsonrpc":"2.0","id":identifier,"method":method,"params":params}).encode()
            with opener.open(Request(origin+"/mcp",data=body,headers=headers,method="POST"),timeout=20) as response:
                assert response.headers["Cache-Control"]=="no-store"
                result=json.load(response)
            assert result["id"]==identifier and "error" not in result
            assert result["result"].get("isError") is not True
            results.append(result["result"])
        assert "2026-07-28" in results[0]["supportedVersions"]
        assert any(tool["name"]=="platform_capabilities" for tool in results[1]["tools"])
        print(json.dumps({"data":results[2]["structuredContent"]["data"],"rpc_calls":3,"discovered_tools":len(results[1]["tools"]),"database_url_is_unusable":True}))
        return 0
    except BaseException as error:
        failure={"error_class":type(error).__name__,"message":str(error),"rpc_calls":len(results)+1}
        if isinstance(error,HTTPError):failure.update(status=error.code,body=error.read(4096).decode("utf-8",errors="replace"))
        for key,value in list(failure.items()):
            if isinstance(value,str):failure[key]=value.replace(credentials["token"],"<redacted>")
        print(json.dumps(failure),file=sys.stderr)
        return 1


if __name__=="__main__":raise SystemExit(main())
