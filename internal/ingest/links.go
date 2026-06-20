package ingest

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
)

type linkMeta struct {
	Title       string
	Description string
	SiteName    string
	ImageURL    string
}

// parseLinkMeta extracts Open Graph / Twitter Card / basic metadata from an HTML
// document, resolving the preview image against baseURL.
func parseLinkMeta(body []byte, baseURL string) linkMeta {
	var m linkMeta
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return m
	}
	var titleTag string

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "title":
				if titleTag == "" && n.FirstChild != nil {
					titleTag = strings.TrimSpace(n.FirstChild.Data)
				}
			case "meta":
				var prop, name, content string
				for _, a := range n.Attr {
					switch strings.ToLower(a.Key) {
					case "property":
						prop = strings.ToLower(a.Val)
					case "name":
						name = strings.ToLower(a.Val)
					case "content":
						content = a.Val
					}
				}
				key := prop
				if key == "" {
					key = name
				}
				switch key {
				case "og:title", "twitter:title":
					setIfEmpty(&m.Title, content)
				case "og:description", "twitter:description", "description":
					setIfEmpty(&m.Description, content)
				case "og:site_name", "application-name":
					setIfEmpty(&m.SiteName, content)
				case "og:image", "og:image:url", "og:image:secure_url", "twitter:image", "twitter:image:src":
					setIfEmpty(&m.ImageURL, content)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	if m.Title == "" {
		m.Title = titleTag
	}
	m.ImageURL = absoluteURL(baseURL, m.ImageURL)
	return m
}

func setIfEmpty(dst *string, v string) {
	if *dst == "" {
		*dst = strings.TrimSpace(v)
	}
}
