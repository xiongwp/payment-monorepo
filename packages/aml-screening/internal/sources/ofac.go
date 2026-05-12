// Package sources — 各国制裁 / PEP 名单的 ingestion adapter.
//
// 实现策略:
//   - 每个 source 一个 Fetch() — 拉原始数据 (HTTP / SFTP / DB)
//   - Parse() — 解析为 []domain.ListEntry
//   - 主流程: Fetch → Parse → Upsert → PurgeStale (delete by source where updated_at < cutoff)
//
// 调度: aml-screening server 启动后跑 ListRefresher (per source), 默认每天一次.
//
// ── OFAC SDN ──
// 数据源: https://www.treasury.gov/ofac/downloads/sdn.xml (XML, ~80MB)
// 频率: 美国财政部每日更新
// 格式: <sdnList><sdnEntry>...</sdnEntry>... </sdnList>

package sources

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"reconcile-system/packages/aml-screening/internal/domain"
)

const OFACSDNURL = "https://www.treasury.gov/ofac/downloads/sdn.xml"

// OFACFetcher 抓 SDN XML
type OFACFetcher struct {
	URL    string
	Client *http.Client
}

func NewOFACFetcher() *OFACFetcher {
	return &OFACFetcher{
		URL:    OFACSDNURL,
		Client: &http.Client{Timeout: 5 * time.Minute},
	}
}

// Fetch 从 OFAC 下载完整 SDN XML.
// 生产建议: 先 HEAD 检查 Last-Modified, 跟上次跑的 timestamp 对比, 没变就 skip.
func (f *OFACFetcher) Fetch() ([]byte, error) {
	req, err := http.NewRequest("GET", f.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "aml-screening/1.0 (compliance)")
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch ofac sdn: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ofac sdn http %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// ── OFAC XML schema (简化) ──
//
// 真实 schema 比这里多很多字段, 但匹配只需要 name/aliases/dob/nationality/addresses/ids.
// 完整 schema: https://www.treasury.gov/ofac/downloads/sdn.pdf

type ofacSDNList struct {
	XMLName xml.Name      `xml:"sdnList"`
	Entries []ofacEntry   `xml:"sdnEntry"`
}

type ofacEntry struct {
	UID         int             `xml:"uid"`
	FirstName   string          `xml:"firstName"`
	LastName    string          `xml:"lastName"`
	SDNType     string          `xml:"sdnType"` // Individual / Entity / Vessel / Aircraft
	ProgramList struct {
		Programs []string `xml:"program"`
	} `xml:"programList"`
	AKAList struct {
		AKAs []ofacAKA `xml:"aka"`
	} `xml:"akaList"`
	DOBList struct {
		DOBs []struct {
			DOB string `xml:"dateOfBirth"`
		} `xml:"dateOfBirthItem"`
	} `xml:"dateOfBirthList"`
	POBList struct {
		POBs []struct {
			POB string `xml:"placeOfBirth"`
		} `xml:"placeOfBirthItem"`
	} `xml:"placeOfBirthList"`
	NationalityList struct {
		Nats []struct {
			Country string `xml:"country"`
		} `xml:"nationality"`
	} `xml:"nationalityList"`
	AddressList struct {
		Addrs []ofacAddr `xml:"address"`
	} `xml:"addressList"`
	IDList struct {
		IDs []ofacID `xml:"id"`
	} `xml:"idList"`
	Remarks string `xml:"remarks"`
}

type ofacAKA struct {
	UID       int    `xml:"uid"`
	Type      string `xml:"type"`
	Category  string `xml:"category"`
	LastName  string `xml:"lastName"`
	FirstName string `xml:"firstName"`
}

type ofacAddr struct {
	UID     int    `xml:"uid"`
	Address1 string `xml:"address1"`
	Address2 string `xml:"address2"`
	City    string `xml:"city"`
	Country string `xml:"country"`
}

type ofacID struct {
	UID       int    `xml:"uid"`
	IDType    string `xml:"idType"`
	IDNumber  string `xml:"idNumber"`
	IDCountry string `xml:"idCountry"`
}

// Parse 解析 OFAC SDN XML → 标准 ListEntry.
func ParseOFACSDN(raw []byte) ([]domain.ListEntry, error) {
	var l ofacSDNList
	if err := xml.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("ofac xml unmarshal: %w", err)
	}
	now := time.Now().UTC()
	out := make([]domain.ListEntry, 0, len(l.Entries))
	for _, e := range l.Entries {
		entry := domain.ListEntry{
			ID:          fmt.Sprintf("OFAC-%d", e.UID),
			Source:      domain.SourceOFACSDN,
			EntityType:  ofacTypeToSubject(e.SDNType),
			PrimaryName: joinName(e.FirstName, e.LastName),
			DOB:         firstDOB(e),
			BirthPlace:  firstPOB(e),
			Program:     strings.Join(e.ProgramList.Programs, ","),
			Remarks:     e.Remarks,
			ListedAt:    now,
			UpdatedAt:   now,
		}
		// aliases
		for _, a := range e.AKAList.AKAs {
			entry.Aliases = append(entry.Aliases, joinName(a.FirstName, a.LastName))
		}
		// nationality
		for _, n := range e.NationalityList.Nats {
			if n.Country != "" {
				entry.Nationality = append(entry.Nationality, n.Country)
			}
		}
		// addresses
		for _, a := range e.AddressList.Addrs {
			entry.Addresses = append(entry.Addresses, joinAddr(a))
		}
		// IDs — 名单原始 ID 是明文, 我们 hash 后存
		for _, id := range e.IDList.IDs {
			entry.IDDocuments = append(entry.IDDocuments, domain.IDDoc{
				Type:    id.IDType,
				NumberH: hashID(id.IDNumber),
				Country: id.IDCountry,
			})
		}
		out = append(out, entry)
	}
	return out, nil
}

func ofacTypeToSubject(t string) domain.SubjectType {
	switch strings.ToLower(t) {
	case "individual":
		return domain.SubjectIndividual
	case "entity":
		return domain.SubjectEntity
	case "vessel":
		return domain.SubjectVessel
	case "aircraft":
		return domain.SubjectAircraft
	default:
		return domain.SubjectEntity
	}
}

func joinName(first, last string) string {
	first = strings.TrimSpace(first)
	last = strings.TrimSpace(last)
	if first == "" {
		return last
	}
	if last == "" {
		return first
	}
	return last + " " + first // OFAC primary order
}

func joinAddr(a ofacAddr) string {
	parts := []string{}
	for _, p := range []string{a.Address1, a.Address2, a.City, a.Country} {
		if s := strings.TrimSpace(p); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

func firstDOB(e ofacEntry) string {
	if len(e.DOBList.DOBs) == 0 {
		return ""
	}
	return strings.TrimSpace(e.DOBList.DOBs[0].DOB)
}

func firstPOB(e ofacEntry) string {
	if len(e.POBList.POBs) == 0 {
		return ""
	}
	return strings.TrimSpace(e.POBList.POBs[0].POB)
}

// hashID — id_number → sha256[:16] hex.
// 跟 matcher.go 里的 hashing 路径对齐, 不存明文.
func hashID(s string) string {
	if s == "" {
		return ""
	}
	return hash16(strings.ToLower(strings.TrimSpace(s)))
}
