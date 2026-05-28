// EU Consolidated Financial Sanctions List 接入。
//
// 数据源（生产）：
//
//	https://webgate.ec.europa.eu/fsd/fsf/public/files/xmlFullSanctionsList_1_1/content?token=<token>
//
// 注意：EU 这个 endpoint 需要预先在 FSF Web Service 申请 token。生产部署要把
// token 放 secret，启动时注入 v.GetString("sanction.eu_token")。
//
// EU XML schema 跟 OFAC 完全不同：顶层 <export><sanctionEntity>，subject 名字
// 在 <nameAlias wholeName="..."> 元素里。下面只解析 individual / entity 通用字段。
//
// 此文件目前提供：
//   - EUDownloader struct（http 下载，TODO: 实测 token 拿到再开）
//   - ParseEUXML（拿样本 XML 给测试用 / parsed result 喂 MemService）

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

// EUDownloader EU FSF XML 抓取器。
type EUDownloader struct {
	URL    string // 完整 URL（含 token）
	Client *http.Client

	mu           sync.Mutex
	lastModified string
}

// NewEUDownloader url 形如 https://webgate.ec.europa.eu/...?token=XXX
func NewEUDownloader(url string) *EUDownloader {
	return &EUDownloader{
		URL:    url,
		Client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (d *EUDownloader) Download(ctx context.Context) ([]*Entry, bool, error) {
	if d.URL == "" {
		return nil, false, errors.New("eu: empty URL (need token-bearing EU FSF URL)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return nil, false, err
	}
	d.mu.Lock()
	if d.lastModified != "" {
		req.Header.Set("If-Modified-Since", d.lastModified)
	}
	d.mu.Unlock()
	req.Header.Set("Accept", "application/xml")
	req.Header.Set("User-Agent", "risk-manage-sanction/1.0")

	resp, err := d.Client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("eu: http %d", resp.StatusCode)
	}
	entries, err := ParseEUXML(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("eu: parse: %w", err)
	}
	d.mu.Lock()
	d.lastModified = resp.Header.Get("Last-Modified")
	d.mu.Unlock()
	return entries, true, nil
}

// euSanctionEntityXML EU FSF XML 的 <sanctionEntity> 元素简化模型。
type euSanctionEntityXML struct {
	LogicalID    string `xml:"logicalId,attr"`
	EUReferenceNumber string `xml:"euReferenceNumber,attr"`
	SubjectType  struct {
		Code string `xml:"code,attr"` // P=Person, E=Enterprise
	} `xml:"subjectType"`
	NameAliases []struct {
		WholeName string `xml:"wholeName,attr"`
		FirstName string `xml:"firstName,attr"`
		LastName  string `xml:"lastName,attr"`
		Function  string `xml:"function,attr"`
		Strong    string `xml:"strong,attr"` // true/false
	} `xml:"nameAlias"`
	BirthDates []struct {
		BirthDate string `xml:"birthdate,attr"` // YYYY-MM-DD
	} `xml:"birthdate"`
	Citizenships []struct {
		CountryISO2 string `xml:"countryIso2Code,attr"`
	} `xml:"citizenship"`
	Regulations []struct {
		Programme string `xml:"programme,attr"`
	} `xml:"regulation"`
}

// ParseEUXML 流式解析 EU FSF XML：<export><sanctionEntity>...
func ParseEUXML(r io.Reader) ([]*Entry, error) {
	dec := xml.NewDecoder(r)
	out := make([]*Entry, 0, 512)
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
		if se.Name.Local != "sanctionEntity" {
			continue
		}
		var raw euSanctionEntityXML
		if err := dec.DecodeElement(&raw, &se); err != nil {
			return nil, err
		}
		e := euEntityToEntry(&raw, addedAt)
		if e == nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func euEntityToEntry(x *euSanctionEntityXML, addedAt time.Time) *Entry {
	// 主名 = 第一个 nameAlias 的 wholeName；其余进 Aliases
	if len(x.NameAliases) == 0 {
		return nil
	}
	primary := strings.TrimSpace(x.NameAliases[0].WholeName)
	if primary == "" {
		primary = strings.TrimSpace(strings.TrimSpace(x.NameAliases[0].FirstName) + " " + strings.TrimSpace(x.NameAliases[0].LastName))
	}
	if primary == "" {
		return nil
	}
	aliases := make([]string, 0, len(x.NameAliases)-1)
	for i := 1; i < len(x.NameAliases); i++ {
		a := strings.TrimSpace(x.NameAliases[i].WholeName)
		if a == "" {
			continue
		}
		aliases = append(aliases, a)
	}
	dob := ""
	if len(x.BirthDates) > 0 {
		dob = strings.TrimSpace(x.BirthDates[0].BirthDate)
	}
	country := ""
	if len(x.Citizenships) > 0 {
		country = strings.TrimSpace(x.Citizenships[0].CountryISO2)
	}
	typ := "individual"
	if strings.EqualFold(x.SubjectType.Code, "E") {
		typ = "entity"
	}
	progs := make([]string, 0, len(x.Regulations))
	for _, r := range x.Regulations {
		if p := strings.TrimSpace(r.Programme); p != "" {
			progs = append(progs, p)
		}
	}
	uid := x.LogicalID
	if uid == "" {
		uid = x.EUReferenceNumber
	}
	return &Entry{
		Source:    SourceEU,
		UID:       uid,
		Name:      primary,
		Aliases:   aliases,
		Type:      typ,
		Country:   country,
		BirthDate: dob,
		Programs:  progs,
		AddedAt:   addedAt,
	}
}
