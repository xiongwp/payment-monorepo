// OFAC SDN 名单下载 + XML 解析。
//
// 数据源（生产）：
//
//	https://www.treasury.gov/ofac/downloads/sdn.xml         (full)
//	https://www.treasury.gov/ofac/downloads/sdn.xml.gz      (gzip 压缩)
//	https://www.treasury.gov/ofac/downloads/sdn.xml.zip
//	https://home.treasury.gov/policy-issues/financial-sanctions/specially-designated-nationals-list-data-formats-data-schemas
//
// 设计原则：
//   - HTTP GET 带 If-Modified-Since / If-None-Match，避免反复拉同一份（OFAC 全量 ~70MB）
//   - 304 Not Modified → 返回 (nil, false, nil)，调用方保留旧 entries
//   - 200 OK → 流式 xml.Decoder 解析（避免一次性 OOM）→ 返 ([]*Entry, true, nil)
//   - 失败时调用方保留上一份内存索引，不要清空（合规要求：宁可漏命中不可全空）
//
// 此文件 *只* 做 download + parse；reload 索引由现有 MemService.Reload 完成。

package sanction

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultOFACSDNURL 官方原始 XML（非 gz / 非 zip）。生产建议挂 CDN 镜像。
const DefaultOFACSDNURL = "https://www.treasury.gov/ofac/downloads/sdn.xml"

// OFACDownloader 抓 OFAC SDN XML。并发 safe（mu 保护 etag/lastModified）。
type OFACDownloader struct {
	URL    string
	Client *http.Client

	mu           sync.Mutex
	lastETag     string
	lastModified string // RFC1123 字符串，HTTP If-Modified-Since 头格式
}

// NewOFACDownloader 构造默认 30s timeout 的 downloader。url 空 → 用 DefaultOFACSDNURL。
func NewOFACDownloader(url string) *OFACDownloader {
	if url == "" {
		url = DefaultOFACSDNURL
	}
	return &OFACDownloader{
		URL:    url,
		Client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Download 拉 SDN XML 并解析成 []*Entry。
// 返回 (entries, fresh, err)：fresh=true 表示是新数据（200 OK），fresh=false 表示 304。
// 调用方决定 fresh=false 时是否调 svc.Reload。
func (d *OFACDownloader) Download(ctx context.Context) ([]*Entry, bool, error) {
	if d.URL == "" {
		return nil, false, errors.New("ofac: empty URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return nil, false, err
	}
	d.mu.Lock()
	if d.lastETag != "" {
		req.Header.Set("If-None-Match", d.lastETag)
	}
	if d.lastModified != "" {
		req.Header.Set("If-Modified-Since", d.lastModified)
	}
	d.mu.Unlock()

	req.Header.Set("Accept", "application/xml, text/xml")
	req.Header.Set("User-Agent", "risk-manage-sanction/1.0")

	resp, err := d.Client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, false, nil
	case http.StatusOK:
		// pass
	default:
		// 读 body 前 512B 帮 debug
		buf := make([]byte, 512)
		n, _ := io.ReadFull(resp.Body, buf)
		return nil, false, fmt.Errorf("ofac: http %d: %s", resp.StatusCode, string(buf[:n]))
	}

	entries, err := ParseSDNXML(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("ofac: parse: %w", err)
	}

	d.mu.Lock()
	d.lastETag = resp.Header.Get("ETag")
	d.lastModified = resp.Header.Get("Last-Modified")
	d.mu.Unlock()
	return entries, true, nil
}

// sdnEntryXML 直接对应 SDN XML <sdnEntry> 元素的解析模型。
// 字段按 OFAC SDN human-readable XML schema 取最常用的部分；其余忽略。
type sdnEntryXML struct {
	UID         string `xml:"uid"`
	FirstName   string `xml:"firstName"`
	LastName    string `xml:"lastName"`
	Title       string `xml:"title"`
	SDNType     string `xml:"sdnType"`
	ProgramList struct {
		Programs []string `xml:"program"`
	} `xml:"programList"`
	AKAList struct {
		AKAs []struct {
			Type      string `xml:"type"`      // a.k.a. / f.k.a.
			Category  string `xml:"category"`  // strong / weak
			FirstName string `xml:"firstName"`
			LastName  string `xml:"lastName"`
		} `xml:"aka"`
	} `xml:"akaList"`
	DateOfBirthList struct {
		Items []struct {
			DateOfBirth string `xml:"dateOfBirth"`
		} `xml:"dateOfBirthItem"`
	} `xml:"dateOfBirthList"`
	NationalityList struct {
		Items []struct {
			Country string `xml:"country"`
		} `xml:"nationality"`
	} `xml:"nationalityList"`
	CitizenshipList struct {
		Items []struct {
			Country string `xml:"country"`
		} `xml:"citizenship"`
	} `xml:"citizenshipList"`
}

// ParseSDNXML 流式解析 OFAC SDN XML（顶层 <sdnList><sdnEntry>...</sdnEntry></sdnList>）。
// 用 xml.NewDecoder 逐 token 走 + 在 <sdnEntry> 处 DecodeElement，
// 避免一次性把 ~70MB 文档读进内存。
func ParseSDNXML(r io.Reader) ([]*Entry, error) {
	dec := xml.NewDecoder(r)
	out := make([]*Entry, 0, 1024)
	addedAt := time.Now().UTC()
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if se.Name.Local != "sdnEntry" {
			continue
		}
		var raw sdnEntryXML
		if err := dec.DecodeElement(&raw, &se); err != nil {
			return nil, err
		}
		e := sdnEntryToEntry(&raw, addedAt)
		if e == nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func sdnEntryToEntry(x *sdnEntryXML, addedAt time.Time) *Entry {
	name := joinName(x.FirstName, x.LastName)
	if name == "" {
		return nil
	}
	aliases := make([]string, 0, len(x.AKAList.AKAs))
	for _, a := range x.AKAList.AKAs {
		n := joinName(a.FirstName, a.LastName)
		if n != "" {
			aliases = append(aliases, n)
		}
	}
	country := ""
	if len(x.NationalityList.Items) > 0 {
		country = strings.TrimSpace(x.NationalityList.Items[0].Country)
	} else if len(x.CitizenshipList.Items) > 0 {
		country = strings.TrimSpace(x.CitizenshipList.Items[0].Country)
	}
	dob := ""
	if len(x.DateOfBirthList.Items) > 0 {
		dob = strings.TrimSpace(x.DateOfBirthList.Items[0].DateOfBirth)
	}
	typ := strings.ToLower(strings.TrimSpace(x.SDNType))
	if typ == "" {
		typ = "individual"
	}
	return &Entry{
		Source:    SourceOFAC,
		UID:       strings.TrimSpace(x.UID),
		Name:      name,
		Aliases:   aliases,
		Type:      typ,
		Country:   country,
		BirthDate: dob,
		Programs:  append([]string(nil), x.ProgramList.Programs...),
		AddedAt:   addedAt,
	}
}

func joinName(first, last string) string {
	first = strings.TrimSpace(first)
	last = strings.TrimSpace(last)
	switch {
	case first != "" && last != "":
		return first + " " + last
	case last != "":
		return last
	case first != "":
		return first
	default:
		return ""
	}
}
