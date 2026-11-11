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

API versions and cursor pagination
-----------------------------------

Clients constructed without an explicit version option keep using the legacy
v1 contract: no version header is sent, and every existing method keeps its
historical signature and behavior.

Opting in to API v2 adds new entry fields (`entry_id`, `external_url`,
`excerpt`, `image_url`, `has_media`) and cursor-based pagination:

```go
client := miniflux.NewClientWithOptions(
    "https://api.example.org",
    miniflux.WithAPIKey("my-secret-token"),
    miniflux.WithAPIVersion(miniflux.APIVersion2),
)

filter := &miniflux.EntryPageFilterV2{
    Status:    "unread",
    Order:     "published_at",
    Direction: "desc",
    Limit:     100,
    WithTotal: true, // optional; omit it for pure page walks
}

for {
    page, err := client.EntriesV2(filter)
    if err != nil {
        // ErrV2Required means the client was not built with WithAPIVersion.
        fmt.Println(err)
        return
    }

    for _, entry := range page.Entries {
        fmt.Println(entry.EntryID, entry.Excerpt, entry.HasMedia)
    }

    if !page.HasMore {
        break
    }
    filter.Cursor = page.NextCursor // opaque token, do not parse it
}
```

The negotiated version is echoed by the server in the `X-API-Version`
response header. Cursor tokens expire (after 24 hours): on a bad-request
response indicating an expired cursor, restart from the first page without a
cursor.
