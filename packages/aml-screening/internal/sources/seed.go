// seed.go — dev / test 用 seed 几条已知制裁条目, 不依赖外网.
//
// 真生产关闭这个 (env AML_DEV_SEED=0); dev/CI 开启 (AML_DEV_SEED=1).
// seed 条目都是公开的 OFAC SDN 测试数据.

package sources

import (
	"time"

	"reconcile-system/packages/aml-screening/internal/domain"
	"reconcile-system/packages/aml-screening/internal/store"
)

// SeedDev 灌一批已知名单 — 给 dev / smoke test 用.
func SeedDev(s store.Store) error {
	now := time.Now().UTC()
	entries := []domain.ListEntry{
		{
			ID:          "OFAC-12345",
			Source:      domain.SourceOFACSDN,
			EntityType:  domain.SubjectIndividual,
			PrimaryName: "John Doe",
			Aliases:     []string{"Johnny Doe", "J. Doe"},
			DOB:         "1970-01-01",
			Nationality: []string{"IR"},
			Program:     "IRAN",
			ListedAt:    now,
			UpdatedAt:   now,
		},
		{
			ID:          "OFAC-99999",
			Source:      domain.SourceOFACSDN,
			EntityType:  domain.SubjectEntity,
			PrimaryName: "Acme Sanctions Trading Co Ltd",
			Aliases:     []string{"Acme Trading", "Acme Co"},
			Nationality: []string{"KP"},
			Addresses:   []string{"Pyongyang, North Korea"},
			Program:     "DPRK2",
			ListedAt:    now,
			UpdatedAt:   now,
		},
		{
			ID:          "EU-100001",
			Source:      domain.SourceEUConsolidated,
			EntityType:  domain.SubjectIndividual,
			PrimaryName: "Vladimir Testov",
			Aliases:     []string{"V. Testov"},
			DOB:         "1965-05-15",
			Nationality: []string{"RU"},
			Program:     "Russia/Ukraine sanctions",
			ListedAt:    now,
			UpdatedAt:   now,
		},
		{
			ID:          "PEP-001",
			Source:      domain.SourcePEP,
			EntityType:  domain.SubjectIndividual,
			PrimaryName: "Hassan Politico",
			DOB:         "1958-03-12",
			Nationality: []string{"IR"},
			Program:     "PEP - Foreign Government Senior Official",
			ListedAt:    now,
			UpdatedAt:   now,
		},
	}
	for _, e := range entries {
		if err := s.UpsertEntry(e); err != nil {
			return err
		}
	}
	return nil
}
