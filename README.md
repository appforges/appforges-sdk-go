# AppForges Go SDK

Official standard-library-only SDK. Requires Go 1.23 or newer.

Install: `go get github.com/appforges/appforges-sdk-go@v0.61.0`

Generated from the matching OpenAPI specification tag; JSON fields retain wire snake_case names.

```go
client, err := appforges.NewClient(runtime.WithBearerToken(os.Getenv("APPFORGES_ACCESS_TOKEN")))
if err != nil { panic(err) }
defer client.Close()
profile, err := client.Users.GetProfile(context.Background())
```

Import `appforges "github.com/appforges/appforges-sdk-go"` and `"github.com/appforges/appforges-sdk-go/runtime"`. Every API accepts a context. Errors support `errors.As`; retries honor Retry-After. Optional fields use runtime.Some and runtime.Null to distinguish absence and null. Pagination uses iter.Seq2, and SSE closes connections on early termination.
