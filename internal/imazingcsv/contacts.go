package imazingcsv

import (
	"fmt"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
)

func importContacts(st *store.Store, path string) (matched, total int, retErr error) {
	contacts, err := vcard.ParseFile(path)
	if err != nil {
		return 0, 0, fmt.Errorf("parse iMazing contacts: %w", err)
	}
	for _, contact := range contacts {
		if contact.FullName == "" {
			continue
		}
		total++
		contactMatched := false
		for _, phone := range contact.Phones {
			updated, err := st.UpdateParticipantDisplayNameByPhone(phone, contact.FullName)
			if err != nil {
				return matched, total, fmt.Errorf("update contact phone %q: %w", phone, err)
			}
			if updated {
				contactMatched = true
			}
		}
		for _, email := range contact.Emails {
			updated, err := st.UpdateParticipantDisplayNameByEmail(email, contact.FullName)
			if err != nil {
				return matched, total, fmt.Errorf("update contact email %q: %w", email, err)
			}
			if updated {
				contactMatched = true
			}
		}
		if contactMatched {
			matched++
		}
	}
	return matched, total, nil
}
