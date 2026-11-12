// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/*
Package client implements a client library for the Miniflux REST API.

# Examples

This example fetches the list of users:

	import (
		miniflux "miniflux.app/v2/client"
	)

	client := miniflux.NewClient("https://api.example.org", "admin", "secret")
	users, err := client.Users()
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(users, err)

This example discovers subscriptions on a website:

	subscriptions, err := client.Discover("https://example.org/")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(subscriptions)

This example previews the recent entries of discovered feed URLs. A feed that
cannot be fetched is returned with an error instead of failing the whole
request:

	feedURLs := make([]string, 0, len(subscriptions))
	for _, subscription := range subscriptions {
		feedURLs = append(feedURLs, subscription.URL)
	}
	previews, err := client.FeedPreviews(feedURLs)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(previews)
*/
package client // import "miniflux.app/v2/client"
