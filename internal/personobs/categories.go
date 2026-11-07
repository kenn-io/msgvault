package personobs

import (
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// The English lexicon supplements model tags. Its order is the category order,
// so detection produces a stable set even when a phrase matches several terms.
var categoryLexicon = []struct {
	category Category
	words    *regexp.Regexp
}{
	{CategoryHealth, regexp.MustCompile(`\b(chemotherap(y|ies)|cancers?|diagnos(is|es|ed|ing|e)|diabetes|pregnan(t|cy|cies)|therap(y|ies|ists?)|medications?|hospitals?|hospitali[sz](ed|ing|ation|ations|e|es)|surger(y|ies)|hiv|depressions?|anxiety)\b`)},
	{CategorySexualOrientationOrSexLife, regexp.MustCompile(`\b(gays?|lesbians?|bisexuals?|sexual orientations?|sex (life|lives)|sexuality|homosexuals?|asexuals?)\b`)},
	{CategoryGenderIdentity, regexp.MustCompile(`\b(transgender|nonbinary|non-binary|gender identit(y|ies)|gender transition(s|ed|ing)?)\b`)},
	{CategoryReligionOrBelief, regexp.MustCompile(`\b(religions?|religious|christians?|muslims?|jewish|hindus?|buddhists?|atheists?|church(es)?|mosques?|synagogues?)\b`)},
	{CategoryPolitics, regexp.MustCompile(`\b((votes?|voted|voting) for|political|politics|democrats?|republicans?|socialists?|conservative part(y|ies)|labour part(y|ies))\b`)},
	{CategoryEthnicityOrRace, regexp.MustCompile(`\b(ethnicit(y|ies)|ethnic|races?|racial|african americans?|indigenous)\b`)},
	{CategoryTradeUnion, regexp.MustCompile(`\b(trade unions?|labor unions?|labour unions?|union members?|union memberships?)\b`)},
	{CategoryGeneticOrBiometric, regexp.MustCompile(`\b(genetic|genomes?|dna|biometrics?|fingerprints?)\b`)},
	{CategoryCriminalRecord, regexp.MustCompile(`\b(criminal records?|convict(ed|ing|ions?)|arrest(ed|ing|s)?|prisons?|parole|felon(y|ies))\b`)},
	{CategoryImmigrationStatus, regexp.MustCompile(`\b(immigration status|undocumented|deport(ation|ations|ed|ing)|asylum|refugees?|visas?|permanent residents?)\b`)},
	{CategoryFinancialHardship, regexp.MustCompile(`\b(bankrupt(cy|cies)?|evict(ed|ing|ions?)|financial hardships?|cannot afford|can't afford|insolvent)\b`)},
	{CategoryMinors, regexp.MustCompile(`\b(child|children|daughters?|sons?|kids?|minors?|kindergartens?|schoolchild|schoolchildren|teenagers?)\b`)},
	{CategoryFinances, regexp.MustCompile(`\b(mortgages?|salar(y|ies)|incomes?|finances|financial|debts?|loans?|investments?|pensions?)\b`)},
	{CategoryLegalProceedings, regexp.MustCompile(`\b(lawsuits?|litigation|legal proceedings?|court cases?|divorc(e|es|ed|ing)|sued|suing)\b`)},
}

func DetectCategories(content string) []Category {
	var out []Category
	content = strings.NewReplacer("’", "'", "‘", "'", "ʼ", "'").Replace(norm.NFKC.String(content))
	content = strings.ToLower(content)
	for _, entry := range categoryLexicon {
		if entry.words.MatchString(content) {
			out = append(out, entry.category)
		}
	}
	return out
}
func (c Category) Valid() bool {
	for _, entry := range categoryLexicon {
		if c == entry.category {
			return true
		}
	}
	return false
}
func (c Category) Special() bool {
	return c.Valid() && c != CategoryFinances && c != CategoryLegalProceedings
}
