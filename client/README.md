Miniflux API Client
===================

[![PkgGoDev](https://pkg.go.dev/badge/miniflux.app/v2/client)](https://pkg.go.dev/miniflux.app/v2/client)

Go client for the Miniflux REST API. It supports API tokens or basic authentication and mirrors the server endpoints closely.

Installation
------------

```bash
go get -u miniflux.app/v2/client
```

Example
-------

```go
package main

import (
	"fmt"
	"os"

	miniflux "miniflux.app/v2/client"
)

func main() {
    // Authentication with username/password:
    client := miniflux.NewClient("https://api.example.org", "admin", "secret")

    // Authentication with an API Key:
    client := miniflux.NewClient("https://api.example.org", "my-secret-token")

    // Fetch all feeds.
    feeds, err := client.Feeds()
    if err != nil {
        fmt.Println(err)
        return
    }
    fmt.Println(feeds)

    // Backup your feeds to an OPML file.
    opml, err := client.Export()
    if err != nil {
        fmt.Println(err)
        return
    }

    err = os.WriteFile("opml.xml", opml, 0644)
    if err != nil {
        fmt.Println(err)
        return
    }
}
```

Optimistic concurrency control
-------------------------------

Every entry carries a monotonic `Version`, and entry responses expose it as
an `ETag` header (`W/"entry-version-<n>"`). Pass the ETag back through
`If-Match` (or the `expected_version` JSON field) to make a write
conditional: when another client modified the entry in the meantime, the
server rejects the request with HTTP 409 and returns the current ETag
instead of silently overwriting the change. Requests without a version keep
the legacy unconditional behavior.

```go
package main

import (
    "context"
    "errors"
    "fmt"

    miniflux "miniflux.app/v2/client"
)

func renameEntry(ctx context.Context, c *miniflux.Client, entryID int64, title string) error {
    entry, err := c.EntryContext(ctx, entryID)
    if err != nil {
        return err
    }

    _, err = c.UpdateEntryWithETag(ctx, entryID, &miniflux.EntryModificationRequest{
        Title: &title,
    }, entry.ETag())
    if err != nil {
        var conflict *miniflux.EntryConflictError
        if errors.As(err, &conflict) {
            // Another client won the race. Refresh the entry, reapply the
            // local change, and retry.
            return fmt.Errorf("entry changed concurrently; current ETag: %s", conflict.CurrentETag)
        }
        return err
    }
    return nil
}
```

