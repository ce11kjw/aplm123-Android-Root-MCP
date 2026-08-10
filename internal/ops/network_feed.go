package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"

	"golang.org/x/net/html/charset"
)

type rssDocument struct {
	XMLName xml.Name
	Channel struct {
		Title       string    `xml:"title"`
		Link        string    `xml:"link"`
		Description string    `xml:"description"`
		Items       []rssItem `xml:"item"`
	} `xml:"channel"`
	Items []rssItem `xml:"item"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	Description string `xml:"description"`
	Published   string `xml:"pubDate"`
	Date        string `xml:"date"`
	Author      string `xml:"author"`
	Creator     string `xml:"creator"`
}

type atomDocument struct {
	Title    string      `xml:"title"`
	Subtitle string      `xml:"subtitle"`
	Links    []atomLink  `xml:"link"`
	Entries  []atomEntry `xml:"entry"`
}

type atomEntry struct {
	Title     string       `xml:"title"`
	ID        string       `xml:"id"`
	Summary   string       `xml:"summary"`
	Content   string       `xml:"content"`
	Published string       `xml:"published"`
	Updated   string       `xml:"updated"`
	Links     []atomLink   `xml:"link"`
	Authors   []atomAuthor `xml:"author"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

type streamedFeed struct {
	Format      string
	Title       string
	Description string
	Link        string
	RSSItems    []rssItem
	AtomEntries []atomEntry
	Truncated   bool
}

func (m *Manager) parseFeed(ctx context.Context, req Request) Result {
	maxItems, err := argInt64(req.Args, 30, "maxItems")
	if err != nil || maxItems < 1 || maxItems > 200 {
		return invalid("maxItems 必须在 1..200 之间")
	}
	if _, exists := req.Args["maxResponseBytes"]; !exists {
		req.Args["maxResponseBytes"] = int64(4 << 20)
	}
	fetched, err := m.fetchHTTP(ctx, req.Args, 4<<20)
	if err != nil {
		return fail("NETWORK_ERROR", "Feed 读取失败", err, "rss_atom_parse")
	}
	if fetched.Status < 200 || fetched.Status >= 400 {
		return fail("HTTP_ERROR", "Feed 返回错误状态", httpStatusError(fetched.Status), "rss_atom_parse")
	}
	feed, err := decodeFeedStream(fetched.Body, fetched.Headers.Get("Content-Type"), int(maxItems))
	if err != nil {
		return fail("PARSE_ERROR", "Feed XML 解析失败", safeFeedParseError(err), "rss_atom_parse")
	}
	items := make([]map[string]any, 0)
	itemBudget := int(m.cfg.ResultPreviewBytes) - 4096
	if itemBudget < 1024 {
		itemBudget = 1024
	}
	itemBytes := 0
	outputTruncated := feed.Truncated
	metadata := map[string]any{"url": displayHTTPURL(fetched.RequestedURL), "finalUrl": displayHTTPURL(fetched.FinalURL), "cookieJar": fetched.Jar, "responseTruncated": fetched.Truncated, "requestedMaxItems": maxItems, "inlineLimitBytes": m.cfg.ResultPreviewBytes}
	switch feed.Format {
	case "atom":
		metadata["title"] = truncateUTF8Bytes(cleanSpace(feed.Title), 512)
		metadata["description"] = truncateUTF8Bytes(cleanSpace(feed.Description), 1024)
		metadata["link"] = truncateUTF8Bytes(feed.Link, 2048)
		for _, entry := range feed.AtomEntries {
			item := boundedFeedItem(map[string]any{"title": cleanSpace(entry.Title), "link": atomHref(entry.Links), "id": cleanSpace(entry.ID), "summary": cleanSpace(firstNonEmpty(entry.Summary, entry.Content)), "published": cleanSpace(entry.Published), "updated": cleanSpace(entry.Updated), "author": atomAuthorName(entry.Authors)})
			if !appendFeedItem(&items, item, &itemBytes, itemBudget) {
				outputTruncated = true
				break
			}
		}
	case "rss":
		metadata["title"] = truncateUTF8Bytes(cleanSpace(feed.Title), 512)
		metadata["description"] = truncateUTF8Bytes(cleanSpace(feed.Description), 1024)
		metadata["link"] = truncateUTF8Bytes(cleanSpace(feed.Link), 2048)
		for _, entry := range feed.RSSItems {
			item := boundedFeedItem(map[string]any{"title": cleanSpace(entry.Title), "link": cleanSpace(entry.Link), "id": cleanSpace(entry.GUID), "summary": cleanSpace(entry.Description), "published": cleanSpace(firstNonEmpty(entry.Published, entry.Date)), "author": cleanSpace(firstNonEmpty(entry.Author, entry.Creator))})
			if !appendFeedItem(&items, item, &itemBytes, itemBudget) {
				outputTruncated = true
				break
			}
		}
	default:
		return fail("UNSUPPORTED_FEED", "XML 不是受支持的 RSS/Atom Feed", errors.New(feed.Format), "rss_atom_probe")
	}
	metadata["format"] = feed.Format
	metadata["items"] = items
	metadata["count"] = len(items)
	metadata["truncated"] = outputTruncated || fetched.Truncated
	fitFeedResultData(metadata, int(m.cfg.ResultPreviewBytes))
	return ok("RSS/Atom 解析成功", metadata, "rss_atom_parse")
}

func fitFeedResultData(data map[string]any, budget int) {
	if budget < 1024 || jsonEncodedSize(data) <= budget {
		return
	}
	data["truncated"] = true
	description := fmt.Sprint(data["description"])
	link := fmt.Sprint(data["link"])
	title := fmt.Sprint(data["title"])
	data["description"] = ""
	data["link"] = ""
	if items, ok := data["items"].([]map[string]any); ok {
		for len(items) > 0 && jsonEncodedSize(data) > budget {
			items = items[:len(items)-1]
			data["items"] = items
			data["count"] = len(items)
		}
	}
	if jsonEncodedSize(data) > budget && !fitJSONStringField(data, "title", title, budget) {
		if raw, ok := data["url"].(string); ok {
			data["url"] = displayHTTPAuthority(raw)
		}
		if raw, ok := data["finalUrl"].(string); ok {
			data["finalUrl"] = displayHTTPAuthority(raw)
		}
		if !fitJSONStringField(data, "title", title, budget) {
			format := data["format"]
			for key := range data {
				delete(data, key)
			}
			data["format"] = format
			data["items"] = []map[string]any{}
			data["count"] = 0
			data["truncated"] = true
			data["inlineLimitBytes"] = budget
			return
		}
	}
	_ = fitJSONStringField(data, "link", link, budget)
	_ = fitJSONStringField(data, "description", description, budget)
}

func decodeFeedStream(raw []byte, contentType string, maxItems int) (streamedFeed, error) {
	decoder, err := feedXMLDecoder(raw, contentType)
	if err != nil {
		return streamedFeed{}, err
	}
	result := streamedFeed{}
	depth := 0
	channelDepth := -1
	atomLinks := make([]atomLink, 0, 4)
	for {
		token, tokenErr := decoder.Token()
		if errors.Is(tokenErr, io.EOF) {
			break
		}
		if tokenErr != nil {
			return streamedFeed{}, tokenErr
		}
		switch typed := token.(type) {
		case xml.StartElement:
			local := strings.ToLower(typed.Name.Local)
			if result.Format == "" {
				switch local {
				case "feed":
					result.Format = "atom"
				case "rss", "rdf":
					result.Format = "rss"
				default:
					return streamedFeed{}, errors.New("unsupported feed root: " + typed.Name.Local)
				}
			}
			if result.Format == "rss" {
				if local == "channel" {
					channelDepth = depth + 1
				}
				if local == "item" {
					if len(result.RSSItems) >= maxItems {
						result.Truncated = true
						if err := decoder.Skip(); err != nil {
							return streamedFeed{}, err
						}
						continue
					}
					var item rssItem
					if err := decoder.DecodeElement(&item, &typed); err != nil {
						return streamedFeed{}, err
					}
					result.RSSItems = append(result.RSSItems, item)
					continue
				}
				if channelDepth >= 0 && depth == channelDepth {
					switch local {
					case "title", "description", "link":
						var value string
						if err := decoder.DecodeElement(&value, &typed); err != nil {
							return streamedFeed{}, err
						}
						switch local {
						case "title":
							result.Title = value
						case "description":
							result.Description = value
						case "link":
							result.Link = value
						}
						continue
					}
				}
			} else if result.Format == "atom" {
				if local == "entry" {
					if len(result.AtomEntries) >= maxItems {
						result.Truncated = true
						if err := decoder.Skip(); err != nil {
							return streamedFeed{}, err
						}
						continue
					}
					var entry atomEntry
					if err := decoder.DecodeElement(&entry, &typed); err != nil {
						return streamedFeed{}, err
					}
					result.AtomEntries = append(result.AtomEntries, entry)
					continue
				}
				if depth == 1 {
					switch local {
					case "title", "subtitle":
						var value string
						if err := decoder.DecodeElement(&value, &typed); err != nil {
							return streamedFeed{}, err
						}
						if local == "title" {
							result.Title = value
						} else {
							result.Description = value
						}
						continue
					case "link":
						var link atomLink
						if err := decoder.DecodeElement(&link, &typed); err != nil {
							return streamedFeed{}, err
						}
						if len(atomLinks) < 32 {
							atomLinks = append(atomLinks, link)
						}
						continue
					}
				}
			}
			depth++
		case xml.EndElement:
			if channelDepth == depth && strings.EqualFold(typed.Name.Local, "channel") {
				channelDepth = -1
			}
			if depth > 0 {
				depth--
			}
		}
	}
	if result.Format == "atom" {
		result.Link = atomHref(atomLinks)
	}
	return result, nil
}

func feedXMLDecoder(raw []byte, contentType string) (*xml.Decoder, error) {
	if _, parameters, err := mime.ParseMediaType(contentType); err == nil {
		if label := strings.TrimSpace(parameters["charset"]); label != "" {
			if len(label) > 128 {
				return nil, errors.New("feed charset label exceeds 128 bytes")
			}
			normalized := strings.ToLower(strings.ReplaceAll(label, "_", "-"))
			if normalized != "utf-8" && normalized != "utf8" {
				reader, err := charset.NewReaderLabel(label, bytes.NewReader(raw))
				if err != nil {
					return nil, err
				}
				decoded, err := io.ReadAll(io.LimitReader(reader, (16<<20)+1))
				if err != nil {
					return nil, err
				}
				if len(decoded) > 16<<20 {
					return nil, errors.New("decoded feed exceeds the 16 MiB limit")
				}
				raw = decoded
			}
			raw = normalizeXMLDeclarationEncoding(raw)
		}
	}
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	decoder.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
		if len(label) > 128 {
			return nil, errors.New("feed charset label exceeds 128 bytes")
		}
		return charset.NewReaderLabel(label, input)
	}
	return decoder, nil
}

func safeFeedParseError(err error) error {
	if err == nil {
		return nil
	}
	var syntaxError *xml.SyntaxError
	if errors.As(err, &syntaxError) {
		return &sanitizedNetworkCause{message: fmt.Sprintf("feed XML syntax error at line %d", syntaxError.Line), cause: err}
	}
	// Charset and decoder errors can quote response-controlled labels or XML
	// fragments. Keep the cause for errors.Is/errors.As without returning its
	// uncontrolled text to MCP clients.
	return &sanitizedNetworkCause{message: "feed XML or charset is unsupported", cause: err}
}

func normalizeXMLDeclarationEncoding(raw []byte) []byte {
	limit := len(raw)
	if limit > 512 {
		limit = 512
	}
	prefix := string(raw[:limit])
	end := strings.Index(prefix, "?>")
	if end < 0 {
		return raw
	}
	lower := strings.ToLower(prefix[:end])
	encoding := strings.Index(lower, "encoding")
	if encoding < 0 {
		return raw
	}
	equals := strings.Index(lower[encoding+len("encoding"):], "=")
	if equals < 0 {
		return raw
	}
	equals += encoding + len("encoding")
	valueStart := equals + 1
	for valueStart < end && (prefix[valueStart] == ' ' || prefix[valueStart] == '\t' || prefix[valueStart] == '\r' || prefix[valueStart] == '\n') {
		valueStart++
	}
	if valueStart >= end || prefix[valueStart] != '\'' && prefix[valueStart] != '"' {
		return raw
	}
	quote := prefix[valueStart]
	valueEnd := strings.IndexByte(prefix[valueStart+1:end], quote)
	if valueEnd < 0 {
		return raw
	}
	valueEnd += valueStart + 1
	normalized := make([]byte, 0, len(raw)+5-(valueEnd-valueStart-1))
	normalized = append(normalized, raw[:valueStart+1]...)
	normalized = append(normalized, "UTF-8"...)
	normalized = append(normalized, raw[valueEnd:]...)
	return normalized
}

func boundedFeedItem(item map[string]any) map[string]any {
	limits := map[string]int{"title": 500, "link": 2048, "id": 512, "summary": 2048, "published": 128, "updated": 128, "author": 512}
	bounded := make(map[string]any, len(item))
	for key, value := range item {
		text, _ := value.(string)
		bounded[key] = truncateUTF8Bytes(text, limits[key])
	}
	return bounded
}

func appendFeedItem(items *[]map[string]any, item map[string]any, used *int, budget int) bool {
	raw, _ := json.Marshal(item)
	if *used+len(raw) > budget {
		if summary, ok := item["summary"].(string); ok && len(summary) > 512 {
			item["summary"] = truncateUTF8Bytes(summary, 512)
			raw, _ = json.Marshal(item)
		}
	}
	if *used+len(raw) > budget {
		return false
	}
	*items = append(*items, item)
	*used += len(raw)
	return true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func atomHref(links []atomLink) string {
	for _, link := range links {
		if link.Rel == "" || link.Rel == "alternate" {
			return strings.TrimSpace(link.Href)
		}
	}
	if len(links) > 0 {
		return strings.TrimSpace(links[0].Href)
	}
	return ""
}

func atomAuthorName(authors []atomAuthor) string {
	if len(authors) == 0 {
		return ""
	}
	return cleanSpace(authors[0].Name)
}
