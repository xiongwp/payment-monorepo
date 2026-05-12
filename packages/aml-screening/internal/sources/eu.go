// eu.go — EU Consolidated Financial Sanctions XML.
//
// 数据源: https://webgate.ec.europa.eu/europeaid/fsd/fsf/public/files/xmlFullSanctionsList_1_1/content?token=...
// (实际 URL 需要 EU SAS portal 注册 + 拿 token, 这里抽接口, 配置 yaml 传 URL)
//
// 格式 (简化, 跟 OFAC 同模式):
//   <export><sanctionEntity><nameAlias name="..."/><birthDate.../><identification.../></sanctionEntity></export>

package sources

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"reconcile-system/packages/aml-screening/internal/domain"
)

type euExport struct {
	XMLName  xml.Name     `xml:"export"`
	Entities []euEntity   `xml:"sanctionEntity"`
}

type euEntity struct {
	LogicalID    int           `xml:"logicalId,attr"`
	EUReference  string        `xml:"euReferenceNumber,attr"`
	Designation  []euDesign    `xml:"designation"`
	NameAliases  []euNameAlias `xml:"nameAlias"`
	BirthDates   []euBirth     `xml:"birthDate"`
	Citizenships []euCitizen   `xml:"citizenship"`
	Addresses    []euAddr      `xml:"address"`
	IDs          []euID        `xml:"identification"`
	Subject      string        `xml:"subjectType>code,attr"` // P=person, E=enterprise
}

type euDesign struct {
	Designation string `xml:",chardata"`
}

type euNameAlias struct {
	WholeName string `xml:"wholeName,attr"`
	FirstName string `xml:"firstName,attr"`
	LastName  string `xml:"lastName,attr"`
}

type euBirth struct {
	BirthDate string `xml:"birthdate,attr"`
	Country   string `xml:"countryDescription,attr"`
}

type euCitizen struct {
	CountryISO string `xml:"countryIso2Code,attr"`
}

type euAddr struct {
	Street    string `xml:"street,attr"`
	City      string `xml:"city,attr"`
	ZipCode   string `xml:"zipCode,attr"`
	Country   string `xml:"countryIso2Code,attr"`
}

type euID struct {
	IDType    string `xml:"identificationTypeCode,attr"`
	Number    string `xml:"number,attr"`
	Country   string `xml:"countryIso2Code,attr"`
}

func ParseEUConsolidated(raw []byte) ([]domain.ListEntry, error) {
	var ex euExport
	if err := xml.Unmarshal(raw, &ex); err != nil {
		return nil, fmt.Errorf("eu xml unmarshal: %w", err)
	}
	now := time.Now().UTC()
	out := make([]domain.ListEntry, 0, len(ex.Entities))
	for _, e := range ex.Entities {
		primary := ""
		var aliases []string
		for i, a := range e.NameAliases {
			name := a.WholeName
			if name == "" {
				name = strings.TrimSpace(a.FirstName + " " + a.LastName)
			}
			if i == 0 {
				primary = name
				continue
			}
			aliases = append(aliases, name)
		}
		entry := domain.ListEntry{
			ID:          fmt.Sprintf("EU-%d", e.LogicalID),
			Source:      domain.SourceEUConsolidated,
			EntityType:  euSubject(e.Subject),
			PrimaryName: primary,
			Aliases:     aliases,
			Program:     strings.TrimSpace(joinDesign(e.Designation)),
			ListedAt:    now,
			UpdatedAt:   now,
		}
		if len(e.BirthDates) > 0 {
			entry.DOB = e.BirthDates[0].BirthDate
			entry.BirthPlace = e.BirthDates[0].Country
		}
		for _, c := range e.Citizenships {
			if c.CountryISO != "" {
				entry.Nationality = append(entry.Nationality, c.CountryISO)
			}
		}
		for _, a := range e.Addresses {
			parts := []string{}
			for _, p := range []string{a.Street, a.ZipCode, a.City, a.Country} {
				if s := strings.TrimSpace(p); s != "" {
					parts = append(parts, s)
				}
			}
			entry.Addresses = append(entry.Addresses, strings.Join(parts, ", "))
		}
		for _, id := range e.IDs {
			entry.IDDocuments = append(entry.IDDocuments, domain.IDDoc{
				Type:    id.IDType,
				NumberH: hash16(strings.ToLower(strings.TrimSpace(id.Number))),
				Country: id.Country,
			})
		}
		out = append(out, entry)
	}
	return out, nil
}

func joinDesign(ds []euDesign) string {
	parts := []string{}
	for _, d := range ds {
		if s := strings.TrimSpace(d.Designation); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "; ")
}

func euSubject(code string) domain.SubjectType {
	switch strings.ToUpper(code) {
	case "P":
		return domain.SubjectIndividual
	case "E":
		return domain.SubjectEntity
	default:
		return domain.SubjectEntity
	}
}
