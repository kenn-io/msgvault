package whatsapp

import (
	"fmt"

	"github.com/wesm/msgvault/internal/store"
	"github.com/wesm/msgvault/internal/vcard"
)

// ImportContacts reads a .vcf file and updates participant display names
// for any phone numbers that match existing participants in the store.
// Only updates existing participants — does not create new ones.
// Returns the number of existing participants whose names were updated.
func ImportContacts(s *store.Store, vcfPath string) (matched, total int, err error) {
	contacts, err := vcard.ParseFile(vcfPath)
	if err != nil {
		return 0, 0, fmt.Errorf("parse vcard: %w", err)
	}

	total = len(contacts)
	var errCount int
	for _, c := range contacts {
		if c.FullName == "" {
			continue
		}
		for _, phone := range c.Phones {
			if phone == "" {
				continue
			}
			updated, updateErr := s.UpdateParticipantDisplayNameByPhone(phone, c.FullName)
			if updateErr != nil {
				errCount++
				continue
			}
			if updated {
				matched++
			}
		}
	}

	if errCount > 0 {
		return matched, total, fmt.Errorf("contact import completed with %d database errors", errCount)
	}

	return matched, total, nil
}
