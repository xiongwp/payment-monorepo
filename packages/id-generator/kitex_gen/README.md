# kitex_gen — generated stubs (do not edit)

This directory hosts Kitex-generated server / client stubs. **Do not edit by hand.**

Generate command (from monorepo root):

```bash
./idl/generate.sh idgen
```

That will produce:

- `idgen/v1/idgen.pb.go` — protobuf messages
- `idgen/v1/idgenservice/idgenservice.go` — server.NewServer wrapper
- `idgen/v1/idgenservice/client.go` — client.NewClient wrapper

After generation, `cmd/server/main.go` references:

```go
import idgenservice "reconcile-system/packages/id-generator/kitex_gen/idgen/v1/idgenservice"
```
