package personobs

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDetectCategories(t *testing.T) {
	assertions := assert.New(t)
	for _, tt := range []struct {
		text string
		want []Category
	}{
		{"[PERSON] is receiving chemotherapy", []Category{CategoryHealth}},
		{"[PERSON] voted for a candidate", []Category{CategoryPolitics}},
		{"[PERSON] has a mortgage", []Category{CategoryFinances}},
		{"[PERSON] filed a lawsuit", []Category{CategoryLegalProceedings}},
		{"[PERSON] runs marathons", nil},
		{"[PERSON] discusses chemotherapy and a mortgage", []Category{CategoryHealth, CategoryFinances}},
		{"[PERSON] studies nonpolitical morphology", nil},
	} {
		assertions.Equal(tt.want, DetectCategories(tt.text), tt.text)
	}
}
func TestCategorySpecial(t *testing.T) {
	assertions := assert.New(t)
	assertions.True(CategoryHealth.Special())
	assertions.False(CategoryFinances.Special())
	assertions.False(CategoryLegalProceedings.Special())
	assertions.False(Category("unknown").Special())
}

func TestDetectCategoriesInflectionsAndUnicode(t *testing.T) {
	assertions := assert.New(t)
	for _, tt := range []struct {
		text string
		want Category
	}{
		{"medications", CategoryHealth}, {"surgeries", CategoryHealth}, {"hospitalized", CategoryHealth}, {"hospitalised", CategoryHealth}, {"hospitalizing", CategoryHealth}, {"diagnosing", CategoryHealth},
		{"mortgages", CategoryFinances}, {"loans", CategoryFinances}, {"debts", CategoryFinances}, {"salaries", CategoryFinances},
		{"lawsuits", CategoryLegalProceedings}, {"court cases", CategoryLegalProceedings}, {"divorced", CategoryLegalProceedings},
		{"daughters", CategoryMinors}, {"sons", CategoryMinors}, {"kids", CategoryMinors}, {"kid", CategoryMinors},
		{"religions", CategoryReligionOrBelief}, {"churches", CategoryReligionOrBelief},
		{"votes for", CategoryPolitics}, {"voting for", CategoryPolitics}, {"Democrats", CategoryPolitics},
		{"union members", CategoryTradeUnion}, {"trade unions", CategoryTradeUnion},
		{"fingerprints", CategoryGeneticOrBiometric}, {"convictions", CategoryCriminalRecord}, {"refugees", CategoryImmigrationStatus},
		{"can’t afford", CategoryFinancialHardship}, {"canʼt afford", CategoryFinancialHardship}, {"can＇t afford", CategoryFinancialHardship},
		{"ｍｅｄｉｃａｔｉｏｎｓ", CategoryHealth}, {"ＭＯＲＴＧＡＧＥＳ", CategoryFinances},
	} {
		t.Run(tt.text, func(t *testing.T) {
			assertions := assert.New(t)
			assertions.Contains(DetectCategories(tt.text), tt.want)
		})
	}
	assertions.Empty(DetectCategories("concatenated mortgageable hospitalities kiddish"))
}

func TestDetectCategoriesCollapsesMultiwordWhitespace(t *testing.T) {
	for _, text := range []string{
		"[PERSON] reports financial  hardship",
		"[PERSON] reports financial\thardship",
		"[PERSON] reports financial\nhardship",
	} {
		t.Run(text, func(t *testing.T) {
			assert.Contains(t, DetectCategories(text), CategoryFinancialHardship)
		})
	}
}

func FuzzDetectCategoriesWhitespaceNormalization(f *testing.F) {
	for _, separator := range []string{" ", "  ", "\t", "\n", "\r\n", " \t\n"} {
		f.Add(separator)
	}
	f.Fuzz(func(t *testing.T, separator string) {
		if separator == "" || len(strings.Fields(separator)) != 0 {
			t.Skip()
		}
		assert.Equal(t, DetectCategories("financial hardship"),
			DetectCategories("financial"+separator+"hardship"))
	})
}
