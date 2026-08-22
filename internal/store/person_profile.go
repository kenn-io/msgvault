package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type PersonProfile struct {
	Person        Person               `json:"person"`
	Names         []PersonName         `json:"names"`
	ContactPoints []PersonContactPoint `json:"contact_points"`
	Addresses     []PersonAddress      `json:"addresses"`
	Dates         []PersonDate         `json:"dates"`
	Categories    []PersonCategory     `json:"categories"`
	Media         []PersonMedia        `json:"media"`
}

type PersonProfilePatch struct {
	Names         *PersonNamePatch         `json:"names,omitempty"`
	ContactPoints *PersonContactPointPatch `json:"contact_points,omitempty"`
	Addresses     *PersonAddressPatch      `json:"addresses,omitempty"`
	Dates         *PersonDatePatch         `json:"dates,omitempty"`
	Categories    *PersonCategoryPatch     `json:"categories,omitempty"`
	Media         *PersonMediaPatch        `json:"media,omitempty"`
}

type PersonNamePatch struct {
	Add       []PersonNameInput `json:"add,omitempty"`
	Supersede []int64           `json:"supersede,omitempty"`
}

type PersonContactPointPatch struct {
	Add       []PersonContactPointInput `json:"add,omitempty"`
	Supersede []int64                   `json:"supersede,omitempty"`
}

type PersonAddressPatch struct {
	Add       []PersonAddressInput `json:"add,omitempty"`
	Supersede []int64              `json:"supersede,omitempty"`
}

type PersonDatePatch struct {
	Add       []PersonDateInput `json:"add,omitempty"`
	Supersede []int64           `json:"supersede,omitempty"`
}

type PersonCategoryPatch struct {
	Add       []PersonCategoryInput `json:"add,omitempty"`
	Supersede []int64               `json:"supersede,omitempty"`
}

type PersonMediaPatch struct {
	Add       []PersonMediaInput `json:"add,omitempty"`
	Supersede []int64            `json:"supersede,omitempty"`
}

type PersonProfileHistory struct {
	Person        Person                          `json:"person"`
	Names         []PersonName                    `json:"names"`
	ContactPoints []PersonContactPoint            `json:"contact_points"`
	Addresses     []PersonAddress                 `json:"addresses"`
	Dates         []PersonDate                    `json:"dates"`
	Categories    []PersonCategory                `json:"categories"`
	Media         []PersonMedia                   `json:"media"`
	Observations  []ParticipantContactObservation `json:"observations"`
}

const MaxPersonProfilePatchOperations = 200

var (
	ErrPersonProfilePatchTooLarge = errors.New("person profile patch exceeds the operation limit")
	ErrPersonProfilePatchEmpty    = errors.New("person profile patch contains no operations")
)

func (s *Store) GetPersonProfileContext(
	ctx context.Context, personID int64,
) (*PersonProfile, error) {
	var profile *PersonProfile
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		profile, err = s.getPersonProfileTx(ctx, tx, personID, true)
		return err
	})
	return profile, err
}

func (s *Store) ApplyPersonProfilePatchContext(
	ctx context.Context,
	personID, expectedRevision int64,
	patch PersonProfilePatch,
) (*PersonProfile, error) {
	operations := countPersonProfilePatchOperations(patch)
	if operations == 0 {
		return nil, ErrPersonProfilePatchEmpty
	}
	if operations > MaxPersonProfilePatchOperations {
		return nil, ErrPersonProfilePatchTooLarge
	}
	var profile *PersonProfile
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		var updatedID int64
		err := tx.QueryRowContext(ctx, `UPDATE persons
			SET revision = revision + 1,
			    vcard_projection_revision = vcard_projection_revision + 1,
			    updated_at = `+s.dialect.Now()+`
			WHERE id = ? AND revision = ? RETURNING id`,
			personID, expectedRevision,
		).Scan(&updatedID)
		if errors.Is(err, sql.ErrNoRows) {
			return s.personCASMissTx(ctx, tx, personID)
		}
		if err != nil {
			return fmt.Errorf("update person profile revision: %w", err)
		}
		if err := s.applyPersonProfilePatchTx(ctx, tx, personID, patch); err != nil {
			return err
		}
		profile, err = s.getPersonProfileTx(ctx, tx, updatedID, true)
		return err
	})
	return profile, err
}

func (s *Store) GetPersonProfileHistoryContext(
	ctx context.Context, personID int64,
) (*PersonProfileHistory, error) {
	var history *PersonProfileHistory
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		profile, err := s.getPersonProfileTx(ctx, tx, personID, false)
		if err != nil {
			return err
		}
		observations, err := s.listObservationsForPersonTx(ctx, tx, personID)
		if err != nil {
			return err
		}
		history = &PersonProfileHistory{
			Person: profile.Person, Names: profile.Names,
			ContactPoints: profile.ContactPoints, Addresses: profile.Addresses,
			Dates: profile.Dates, Categories: profile.Categories,
			Media: profile.Media, Observations: observations,
		}
		return nil
	})
	return history, err
}

func countPersonProfilePatchOperations(patch PersonProfilePatch) int {
	count := 0
	if patch.Names != nil {
		count += len(patch.Names.Add) + len(patch.Names.Supersede)
	}
	if patch.ContactPoints != nil {
		count += len(patch.ContactPoints.Add) + len(patch.ContactPoints.Supersede)
	}
	if patch.Addresses != nil {
		count += len(patch.Addresses.Add) + len(patch.Addresses.Supersede)
	}
	if patch.Dates != nil {
		count += len(patch.Dates.Add) + len(patch.Dates.Supersede)
	}
	if patch.Categories != nil {
		count += len(patch.Categories.Add) + len(patch.Categories.Supersede)
	}
	if patch.Media != nil {
		count += len(patch.Media.Add) + len(patch.Media.Supersede)
	}
	return count
}

func (s *Store) applyPersonProfilePatchTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
	patch PersonProfilePatch,
) error {
	if patch.Names != nil {
		for _, id := range patch.Names.Supersede {
			if err := s.supersedePersonNameTx(ctx, tx, personID, id, nil); err != nil {
				return err
			}
		}
		for _, input := range patch.Names.Add {
			if _, err := s.addPersonNameTx(ctx, tx, personID, input); err != nil {
				return err
			}
		}
	}
	if patch.ContactPoints != nil {
		for _, id := range patch.ContactPoints.Supersede {
			if err := s.supersedePersonContactPointTx(ctx, tx, personID, id, nil); err != nil {
				return err
			}
		}
		for _, input := range patch.ContactPoints.Add {
			if _, err := s.addPersonContactPointTx(ctx, tx, personID, input); err != nil {
				return err
			}
		}
	}
	if patch.Addresses != nil {
		for _, id := range patch.Addresses.Supersede {
			if err := s.supersedePersonAddressTx(ctx, tx, personID, id, nil); err != nil {
				return err
			}
		}
		for _, input := range patch.Addresses.Add {
			if _, err := s.addPersonAddressTx(ctx, tx, personID, input); err != nil {
				return err
			}
		}
	}
	if patch.Dates != nil {
		for _, id := range patch.Dates.Supersede {
			if err := s.supersedePersonDateTx(ctx, tx, personID, id, nil); err != nil {
				return err
			}
		}
		for _, input := range patch.Dates.Add {
			if _, err := s.addPersonDateTx(ctx, tx, personID, input); err != nil {
				return err
			}
		}
	}
	if patch.Categories != nil {
		for _, id := range patch.Categories.Supersede {
			if err := s.supersedePersonCategoryTx(ctx, tx, personID, id, nil); err != nil {
				return err
			}
		}
		for _, input := range patch.Categories.Add {
			if _, err := s.addPersonCategoryTx(ctx, tx, personID, input); err != nil {
				return err
			}
		}
	}
	if patch.Media != nil {
		for _, id := range patch.Media.Supersede {
			if err := s.supersedePersonMediaTx(ctx, tx, personID, id, nil); err != nil {
				return err
			}
		}
		for _, input := range patch.Media.Add {
			if _, err := s.addPersonMediaTx(ctx, tx, personID, input); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) getPersonProfileTx(
	ctx context.Context, tx *loggedTx, personID int64, currentOnly bool,
) (*PersonProfile, error) {
	person, err := s.getPersonTx(ctx, tx, personID)
	if err != nil {
		return nil, err
	}
	profile := &PersonProfile{Person: *person}
	if profile.Names, err = s.listPersonNamesTx(ctx, tx, personID, currentOnly); err != nil {
		return nil, err
	}
	if profile.ContactPoints, err = s.listPersonContactPointsTx(ctx, tx, personID, currentOnly); err != nil {
		return nil, err
	}
	if profile.Addresses, err = s.listPersonAddressesTx(ctx, tx, personID, currentOnly); err != nil {
		return nil, err
	}
	if profile.Dates, err = s.listPersonDatesTx(ctx, tx, personID, currentOnly); err != nil {
		return nil, err
	}
	if profile.Categories, err = s.listPersonCategoriesTx(ctx, tx, personID, currentOnly); err != nil {
		return nil, err
	}
	if profile.Media, err = s.listPersonMediaTx(ctx, tx, personID, currentOnly); err != nil {
		return nil, err
	}
	return profile, nil
}
