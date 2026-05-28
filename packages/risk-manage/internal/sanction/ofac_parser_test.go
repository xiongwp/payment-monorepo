package sanction

import (
	"strings"
	"testing"
)

const sampleSDNXML = `<?xml version="1.0" encoding="UTF-8"?>
<sdnList>
  <sdnEntry>
    <uid>12345</uid>
    <lastName>PUTIN</lastName>
    <firstName>Vladimir</firstName>
    <sdnType>Individual</sdnType>
    <programList>
      <program>UKRAINE-EO13662</program>
      <program>RUSSIA-EO14024</program>
    </programList>
    <akaList>
      <aka>
        <type>a.k.a.</type>
        <category>strong</category>
        <lastName>Putin</lastName>
        <firstName>V.V.</firstName>
      </aka>
    </akaList>
    <dateOfBirthList>
      <dateOfBirthItem>
        <dateOfBirth>07 Oct 1952</dateOfBirth>
      </dateOfBirthItem>
    </dateOfBirthList>
    <nationalityList>
      <nationality>
        <country>Russia</country>
      </nationality>
    </nationalityList>
  </sdnEntry>
  <sdnEntry>
    <uid>99999</uid>
    <lastName>ACME EVIL CORP</lastName>
    <sdnType>Entity</sdnType>
    <programList>
      <program>SDGT</program>
    </programList>
  </sdnEntry>
</sdnList>`

func TestParseSDNXML(t *testing.T) {
	entries, err := ParseSDNXML(strings.NewReader(sampleSDNXML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	e := entries[0]
	if e.Source != SourceOFAC {
		t.Errorf("source: got %v", e.Source)
	}
	if e.UID != "12345" {
		t.Errorf("uid: got %q", e.UID)
	}
	if e.Name != "Vladimir PUTIN" {
		t.Errorf("name: got %q", e.Name)
	}
	if len(e.Aliases) != 1 || e.Aliases[0] != "V.V. Putin" {
		t.Errorf("aliases: got %+v", e.Aliases)
	}
	if e.BirthDate != "07 Oct 1952" {
		t.Errorf("dob: got %q", e.BirthDate)
	}
	if e.Country != "Russia" {
		t.Errorf("country: got %q", e.Country)
	}
	if len(e.Programs) != 2 {
		t.Errorf("programs: got %+v", e.Programs)
	}
	if e.Type != "individual" {
		t.Errorf("type: got %q", e.Type)
	}

	// 第二条是 entity
	if entries[1].Type != "entity" {
		t.Errorf("entity type: got %q", entries[1].Type)
	}
	if entries[1].Name != "ACME EVIL CORP" {
		t.Errorf("entity name: got %q", entries[1].Name)
	}
}

func TestParseSDNXML_Empty(t *testing.T) {
	entries, err := ParseSDNXML(strings.NewReader(`<sdnList></sdnList>`))
	if err != nil {
		t.Fatalf("empty list parse: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestParseSDNXML_ParseAndReload(t *testing.T) {
	// 端到端：parse → Reload → Check 命中
	entries, err := ParseSDNXML(strings.NewReader(sampleSDNXML))
	if err != nil {
		t.Fatal(err)
	}
	svc := NewMemService()
	if err := svc.Reload(nil, entries); err != nil {
		t.Fatal(err)
	}
	m := svc.Check(nil, "Vladimir Putin", "")
	if m == nil || m.Hits[0].UID != "12345" {
		t.Fatalf("post-parse check failed: %+v", m)
	}
}

const sampleEUXML = `<?xml version="1.0" encoding="UTF-8"?>
<export>
  <sanctionEntity logicalId="42" euReferenceNumber="EU.42.99">
    <subjectType code="P"/>
    <nameAlias wholeName="Vladimir Vladimirovich Putin" firstName="Vladimir" lastName="Putin" strong="true"/>
    <nameAlias wholeName="V. Putin" firstName="V." lastName="Putin" strong="false"/>
    <birthdate birthdate="1952-10-07"/>
    <citizenship countryIso2Code="RU"/>
    <regulation programme="RU-CRIMEA"/>
  </sanctionEntity>
</export>`

func TestParseEUXML(t *testing.T) {
	entries, err := ParseEUXML(strings.NewReader(sampleEUXML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1, got %d", len(entries))
	}
	e := entries[0]
	if e.Source != SourceEU {
		t.Errorf("source: got %v", e.Source)
	}
	if e.UID != "42" {
		t.Errorf("uid: got %q", e.UID)
	}
	if e.Name != "Vladimir Vladimirovich Putin" {
		t.Errorf("name: got %q", e.Name)
	}
	if len(e.Aliases) != 1 || e.Aliases[0] != "V. Putin" {
		t.Errorf("aliases: got %+v", e.Aliases)
	}
	if e.BirthDate != "1952-10-07" {
		t.Errorf("dob: got %q", e.BirthDate)
	}
	if e.Country != "RU" {
		t.Errorf("country: got %q", e.Country)
	}
	if e.Type != "individual" {
		t.Errorf("type: got %q", e.Type)
	}
	if len(e.Programs) != 1 || e.Programs[0] != "RU-CRIMEA" {
		t.Errorf("programs: got %+v", e.Programs)
	}
}
