package api

import "go.kenn.io/msgvault/internal/authz"

// ScopeSupport describes proven native source enforcement for role principals.
// Legacy grant handlers can filter sources separately; that is not proof here.
type ScopeSupport uint8

const (
	ScopeNone ScopeSupport = iota
	ScopeIndependent
	ScopeFilter
	ScopeObject
)

// OperationPolicy separates role authority from existing credential contracts.
// Mixed-action operations need further handler checks before role admission.
type OperationPolicy struct {
	Permission             authz.Permission
	Sources                ScopeSupport
	AgentReadPermission    authz.Permission
	Delegated              bool
	RemoteRead             bool
	RemoteCollectionsWrite bool
}

func operationPolicy(id string) OperationPolicy {
	if policy, ok := operationPolicies[id]; ok {
		return policy
	}
	return OperationPolicy{Permission: authz.PermissionArchiveAdmin, Sources: ScopeNone}
}

// Every registered API operation is listed, including owner-only operations.
// ScopeNone is deliberate until each native read has a source-leak regression.
// Authenticated health includes account labels, so it is not source-independent.
// AgentReadPermission admits a legacy scoped read; Delegated admits the other
// existing grant-aware handlers. Neither implies native role source support.
var operationPolicies = map[string]OperationPolicy{
	// activity_routes.go
	"createDayEntry":         {Permission: authz.PermissionPersonEdit},
	"deleteDayEntry":         {Permission: authz.PermissionPersonEdit},
	"getActivityDay":         {Permission: authz.PermissionPersonRead},
	"getPersonActivityDay":   {Permission: authz.PermissionPersonRead},
	"getPersonContactState":  {Permission: authz.PermissionPersonRead},
	"listDayEntries":         {Permission: authz.PermissionPersonRead},
	"listPersonActivityDays": {Permission: authz.PermissionPersonRead},

	// attribute_definitions.go
	"createAttributeDefinition": {Permission: authz.PermissionArchiveAdmin},
	"deleteAttributeDefinition": {Permission: authz.PermissionArchiveAdmin},
	"getAttributeDefinition":    {Permission: authz.PermissionPersonRead},
	"listAttributeDefinitions":  {Permission: authz.PermissionPersonRead},
	"patchAttributeDefinition":  {Permission: authz.PermissionArchiveAdmin},

	// calendar_control.go
	"controlCalendar": {Permission: authz.PermissionArchiveAdmin, Delegated: true},

	// carddav.go
	"approveCardDAVPublication":          {Permission: authz.PermissionCardDAVWrite},
	"beginGoogleCardDAVAuthorization":    {Permission: authz.PermissionArchiveAdmin},
	"completeGoogleCardDAVAuthorization": {Permission: authz.PermissionArchiveAdmin},
	"getCardDAVConflict":                 {Permission: authz.PermissionArchiveAdmin},
	"getCardDAVPublication":              {Permission: authz.PermissionPersonRead},
	"getCardDAVStatus":                   {Permission: authz.PermissionArchiveAdmin},
	"listCardDAVBooks":                   {Permission: authz.PermissionArchiveAdmin},
	"listCardDAVConflicts":               {Permission: authz.PermissionArchiveAdmin},
	"listCardDAVConnections":             {Permission: authz.PermissionArchiveAdmin},
	"listCardDAVRuns":                    {Permission: authz.PermissionArchiveAdmin},
	"previewCardDAVPublication":          {Permission: authz.PermissionPersonRead},
	"publishCardDAVPerson":               {Permission: authz.PermissionCardDAVWrite},
	"resolveCardDAVConflict":             {Permission: authz.PermissionCardDAVWrite},
	"saveCardDAVAccount":                 {Permission: authz.PermissionArchiveAdmin},
	"syncCardDAV":                        {Permission: authz.PermissionArchiveAdmin},
	"testCardDAVAccount":                 {Permission: authz.PermissionArchiveAdmin},
	"unpublishCardDAVPerson":             {Permission: authz.PermissionCardDAVWrite},
	"updateCardDAVBookRoles":             {Permission: authz.PermissionArchiveAdmin},

	// cli_account_routes.go
	"updateCLIAccount": {Permission: authz.PermissionArchiveAdmin},

	// cli_collection_routes.go
	"addCLICollectionSources":    {Permission: authz.PermissionCollectionsWrite, RemoteCollectionsWrite: true},
	"createCLICollection":        {Permission: authz.PermissionCollectionsWrite, RemoteCollectionsWrite: true},
	"deleteCLICollection":        {Permission: authz.PermissionCollectionsWrite, RemoteCollectionsWrite: true},
	"removeCLICollectionSources": {Permission: authz.PermissionCollectionsWrite, RemoteCollectionsWrite: true},

	// cli_dedup_routes.go
	"executeCLIDeleteDeduped": {Permission: authz.PermissionDeletionStage},
	"planCLIDeduplicate":      {Permission: authz.PermissionArchiveAdmin},
	"planCLIDeleteDeduped":    {Permission: authz.PermissionDeletionStage},

	// cli_identity_routes.go
	"addCLIIdentity":        {Permission: authz.PermissionArchiveAdmin},
	"discoverCLIIdentities": {Permission: authz.PermissionArchiveAdmin},
	"importCLIIdentities":   {Permission: authz.PermissionArchiveAdmin},
	"listCLIIdentities":     {Permission: authz.PermissionCLIRead, RemoteRead: true},
	"removeCLIIdentity":     {Permission: authz.PermissionArchiveAdmin},

	// communication_services.go
	"createCommunicationService": {Permission: authz.PermissionArchiveAdmin},
	"listCommunicationServices":  {Permission: authz.PermissionPersonRead},

	// document_search.go
	"getCurrentDocumentIndexStatus": {Permission: authz.PermissionSearchRead},

	// employments.go
	"createEmployment":            {Permission: authz.PermissionPersonEdit},
	"deleteEmployment":            {Permission: authz.PermissionPersonEdit},
	"endEmployment":               {Permission: authz.PermissionPersonEdit},
	"getEmployment":               {Permission: authz.PermissionPersonRead},
	"listOrganizationEmployments": {Permission: authz.PermissionPersonRead},
	"listPersonEmployments":       {Permission: authz.PermissionPersonRead},
	"patchEmployment":             {Permission: authz.PermissionPersonEdit},
	"setPrimaryEmployment":        {Permission: authz.PermissionPersonEdit},

	// explore.go
	"countExploreMatches":       {Permission: authz.PermissionSearchRead},
	"explore":                   {Permission: authz.PermissionSearchRead},
	"exploreGroups":             {Permission: authz.PermissionSearchRead},
	"preflightExploreSelection": {Permission: authz.PermissionSearchRead},

	// explore_files.go
	"listExploreFiles": {Permission: authz.PermissionSearchRead},

	// files.go
	"getFile":                {Permission: authz.PermissionAttachmentRead},
	"groupFiles":             {Permission: authz.PermissionSearchRead},
	"searchDomainFiles":      {Permission: authz.PermissionSearchRead},
	"searchFiles":            {Permission: authz.PermissionSearchRead},
	"searchParticipantFiles": {Permission: authz.PermissionSearchRead},
	"searchPersonFiles":      {Permission: authz.PermissionSearchRead},

	// identity_links.go
	"linkIdentityParticipants":   {Permission: authz.PermissionPersonMerge},
	"unlinkIdentityParticipants": {Permission: authz.PermissionPersonMerge},

	// import_jobs.go
	"createImportJob": {Permission: authz.PermissionArchiveAdmin},
	"getImportJob":    {Permission: authz.PermissionArchiveAdmin},

	// kata_issues.go
	"createKataIssue":     {Permission: authz.PermissionCurationWrite},
	"findKataIssues":      {Permission: authz.PermissionSystemRead},
	"getKataIssueContext": {Permission: authz.PermissionSystemRead},
	"linkKataEvidence":    {Permission: authz.PermissionCurationWrite},
	"prepareKataEvidence": {Permission: authz.PermissionCurationWrite},

	// meeting_import.go
	"importMeeting": {Permission: authz.PermissionArchiveAdmin},

	// meetings.go
	"getMeetingContext":      {Permission: authz.PermissionMessageRead},
	"getMeetingMetrics":      {Permission: authz.PermissionMessageRead},
	"listMeetingActionItems": {Permission: authz.PermissionMessageRead},

	// muesli_import.go
	"importMuesli": {Permission: authz.PermissionArchiveAdmin},

	// organizations.go
	"clearOrganizationAttribute":         {Permission: authz.PermissionPersonEdit},
	"createOrganization":                 {Permission: authz.PermissionPersonEdit},
	"deleteOrganization":                 {Permission: authz.PermissionPersonEdit},
	"getOrganization":                    {Permission: authz.PermissionPersonRead},
	"getOrganizationHistory":             {Permission: authz.PermissionPersonRead},
	"getOrganizationProfileMediaContent": {Permission: authz.PermissionPersonRead},
	"listOrganizationAttributes":         {Permission: authz.PermissionPersonRead},
	"listOrganizations":                  {Permission: authz.PermissionPersonRead},
	"mergeOrganization":                  {Permission: authz.PermissionPersonMerge},
	"patchOrganization":                  {Permission: authz.PermissionPersonEdit},
	"putOrganizationProfile":             {Permission: authz.PermissionPersonEdit},
	"setOrganizationAttribute":           {Permission: authz.PermissionPersonEdit},

	// people.go
	"getDomain":                    {Permission: authz.PermissionPersonRead},
	"getDomainContextSummary":      {Permission: authz.PermissionPersonRead},
	"getDomainTimeline":            {Permission: authz.PermissionPersonRead},
	"getParticipant":               {Permission: authz.PermissionPersonRead},
	"getParticipantContextSummary": {Permission: authz.PermissionPersonRead},
	"getParticipantTimeline":       {Permission: authz.PermissionPersonRead},
	"listParticipantInboxes":       {Permission: authz.PermissionPersonRead},
	"searchDomains":                {Permission: authz.PermissionPersonRead},
	"searchParticipants":           {Permission: authz.PermissionPersonRead},

	// people_completion.go
	"completeParticipants": {Permission: authz.PermissionPersonRead},

	// person_agenda.go
	"createPersonAgendaItem":   {Permission: authz.PermissionPersonEdit},
	"getKataIntegrationStatus": {Permission: authz.PermissionSystemRead},
	"linkPersonAgendaItem":     {Permission: authz.PermissionPersonEdit},
	"listPersonAgenda":         {Permission: authz.PermissionPersonRead},
	"unlinkPersonAgendaItem":   {Permission: authz.PermissionPersonEdit},
	"updatePersonAgendaItem":   {Permission: authz.PermissionPersonEdit},

	// person_attributes.go
	"appendPersonNote":     {Permission: authz.PermissionPersonEdit},
	"clearPersonAttribute": {Permission: authz.PermissionPersonEdit, Delegated: true},
	"listPersonAttributes": {Permission: authz.PermissionPersonRead, Delegated: true},
	"setPersonAttribute":   {Permission: authz.PermissionPersonEdit, Delegated: true},

	// person_brief.go
	"generatePersonBrief":      {Permission: authz.PermissionArchiveAdmin},
	"getPersonBrief":           {Permission: authz.PermissionPersonRead},
	"getPersonBriefEnrollment": {Permission: authz.PermissionPersonRead},
	"listPersonBriefVersions":  {Permission: authz.PermissionPersonRead},
	"rejectPersonBrief":        {Permission: authz.PermissionPersonEdit},
	"setPersonBriefEnrollment": {Permission: authz.PermissionArchiveAdmin},

	// person_facts.go
	"listPersonFactClaims":               {Permission: authz.PermissionPersonRead},
	"listPersonFactDecisions":            {Permission: authz.PermissionPersonRead},
	"listPersonFactEvidence":             {Permission: authz.PermissionPersonRead},
	"listPersonFactEvidenceStatusEvents": {Permission: authz.PermissionPersonRead},
	"listPersonFactPins":                 {Permission: authz.PermissionPersonRead},
	"listPersonFactTargets":              {Permission: authz.PermissionPersonRead},
	"setPersonFactPin":                   {Permission: authz.PermissionPersonEdit},

	// person_identities.go
	"listPersonIdentities": {Permission: authz.PermissionPersonRead},

	// person_match_scoring.go
	"getPersonMatchScoringStatus": {Permission: authz.PermissionArchiveAdmin},
	"listPersonMatchJudgments":    {Permission: authz.PermissionArchiveAdmin},
	"personMatchScoringConsent":   {Permission: authz.PermissionArchiveAdmin},
	"personMatchScoringRevoke":    {Permission: authz.PermissionArchiveAdmin},
	"runPersonMatchScoring":       {Permission: authz.PermissionArchiveAdmin},

	// person_merges.go
	"decidePersonMergeCandidate": {Permission: authz.PermissionPersonMerge},
	"getPersonMerge":             {Permission: authz.PermissionPersonRead},
	"getPersonMergeSnapshot":     {Permission: authz.PermissionPersonRead},
	"listPersonMerges":           {Permission: authz.PermissionPersonRead},
	"mergePersons":               {Permission: authz.PermissionPersonMerge, Delegated: true},
	"splitPersonMerge":           {Permission: authz.PermissionPersonMerge},

	// person_network.go
	"getPersonNetwork": {Permission: authz.PermissionPersonRead},

	// person_profile_values.go
	"getPersonProfileHistory":      {Permission: authz.PermissionPersonRead},
	"getPersonProfileMediaContent": {Permission: authz.PermissionPersonRead},
	"getPersonStructuredProfile":   {Permission: authz.PermissionPersonRead, Delegated: true},
	"patchPersonStructuredProfile": {Permission: authz.PermissionPersonEdit, Delegated: true},

	// person_profiles.go
	"createPerson":        {Permission: authz.PermissionPersonEdit},
	"deletePerson":        {Permission: authz.PermissionPersonEdit},
	"getPersonByUID":      {Permission: authz.PermissionPersonRead},
	"getPersonProfile":    {Permission: authz.PermissionPersonRead, Delegated: true},
	"listDirectoryPeople": {Permission: authz.PermissionPersonRead},
	"listPeople":          {Permission: authz.PermissionPersonRead},
	"patchPerson":         {Permission: authz.PermissionPersonEdit, Delegated: true},
	"searchPeople":        {Permission: authz.PermissionPersonRead},

	// person_relationships.go
	"createPersonRelationship":      {Permission: authz.PermissionPersonEdit},
	"createRelationshipType":        {Permission: authz.PermissionPersonEdit},
	"deletePersonRelationship":      {Permission: authz.PermissionPersonEdit},
	"deleteRelationshipType":        {Permission: authz.PermissionPersonEdit},
	"getPersonRelationship":         {Permission: authz.PermissionPersonRead},
	"getRelationshipType":           {Permission: authz.PermissionPersonRead},
	"listPersonRelationshipReviews": {Permission: authz.PermissionPersonRead},
	"listPersonRelationships":       {Permission: authz.PermissionPersonRead},
	"listRelationshipTypes":         {Permission: authz.PermissionPersonRead},
	"patchPersonRelationship":       {Permission: authz.PermissionPersonEdit},
	"patchRelationshipType":         {Permission: authz.PermissionPersonEdit},

	// person_tracking.go
	"getPersonTracking": {Permission: authz.PermissionPersonRead},
	"setPersonTracking": {Permission: authz.PermissionPersonEdit},

	// relationships.go
	"listRelationships": {Permission: authz.PermissionPersonRead},

	// routes.go
	"addAccount":                         {Permission: authz.PermissionArchiveAdmin},
	"beginBackupFreeze":                  {Permission: authz.PermissionArchiveAdmin},
	"buildCLICache":                      {Permission: authz.PermissionArchiveAdmin},
	"cancelDeletion":                     {Permission: authz.PermissionDeletionStage},
	"createCLIDeletionManifest":          {Permission: authz.PermissionDeletionStage},
	"deepSearch":                         {Permission: authz.PermissionSearchRead, AgentReadPermission: authz.PermissionSearchRead},
	"endBackupFreeze":                    {Permission: authz.PermissionArchiveAdmin},
	"fastSearch":                         {Permission: authz.PermissionSearchRead, AgentReadPermission: authz.PermissionSearchRead},
	"filterMessages":                     {Permission: authz.PermissionSearchRead, AgentReadPermission: authz.PermissionSearchRead},
	"findSimilarMessages":                {Permission: authz.PermissionSearchRead},
	"getAgentTokenSelf":                  {Permission: authz.PermissionSystemRead, Sources: ScopeIndependent, Delegated: true},
	"getAggregates":                      {Permission: authz.PermissionSearchRead, AgentReadPermission: authz.PermissionSearchRead},
	"getAttachment":                      {Permission: authz.PermissionAttachmentRead, AgentReadPermission: authz.PermissionAttachmentRead},
	"getAttachmentContent":               {Permission: authz.PermissionAttachmentRead, AgentReadPermission: authz.PermissionAttachmentRead},
	"getCLIAttachment":                   {Permission: authz.PermissionAttachmentRead, AgentReadPermission: authz.PermissionAttachmentRead, RemoteRead: true},
	"getCLICacheStats":                   {Permission: authz.PermissionCLIRead, RemoteRead: true},
	"getCLICollection":                   {Permission: authz.PermissionCLIRead, RemoteRead: true},
	"getCLIMessage":                      {Permission: authz.PermissionMessageRead, AgentReadPermission: authz.PermissionMessageRead, RemoteRead: true},
	"getCLIMessageOriginal":              {Permission: authz.PermissionMessageRead, RemoteRead: true},
	"getCLIMessageRaw":                   {Permission: authz.PermissionMessageRead, RemoteRead: true},
	"getCLIMessageThread":                {Permission: authz.PermissionMessageRead, AgentReadPermission: authz.PermissionMessageRead, RemoteRead: true},
	"getCLIStats":                        {Permission: authz.PermissionStatsRead, AgentReadPermission: authz.PermissionStatsRead, RemoteRead: true},
	"getCacheBuildStatus":                {Permission: authz.PermissionSystemRead},
	"getConversation":                    {Permission: authz.PermissionMessageRead},
	"getDeletion":                        {Permission: authz.PermissionDeletionStage},
	"getDocumentIndexStatus":             {Permission: authz.PermissionSystemRead},
	"getDocumentVectorStatus":            {Permission: authz.PermissionSystemRead},
	"getFileContent":                     {Permission: authz.PermissionAttachmentRead},
	"getGmailIDsByFilter":                {Permission: authz.PermissionSearchRead},
	"getHealth":                          {Permission: authz.PermissionSystemRead, Delegated: true, RemoteRead: true},
	"getIdentityMatchCandidate":          {Permission: authz.PermissionPersonRead},
	"getMediaSearchStatus":               {Permission: authz.PermissionSystemRead},
	"getMessage":                         {Permission: authz.PermissionMessageRead, AgentReadPermission: authz.PermissionMessageRead},
	"getMessageInlinePart":               {Permission: authz.PermissionMessageRead},
	"getOperationRun":                    {Permission: authz.PermissionSystemRead},
	"getOperationStatus":                 {Permission: authz.PermissionSystemRead},
	"getRelationshipCalendar":            {Permission: authz.PermissionPersonRead},
	"getRelationshipTimeline":            {Permission: authz.PermissionPersonRead},
	"getRemoteImage":                     {Permission: authz.PermissionAttachmentRead},
	"getSchedulerStatus":                 {Permission: authz.PermissionSystemRead},
	"getStats":                           {Permission: authz.PermissionStatsRead, AgentReadPermission: authz.PermissionStatsRead},
	"getSubAggregates":                   {Permission: authz.PermissionSearchRead, AgentReadPermission: authz.PermissionSearchRead},
	"getTextAggregates":                  {Permission: authz.PermissionSearchRead},
	"getTextStats":                       {Permission: authz.PermissionStatsRead},
	"getTotalStats":                      {Permission: authz.PermissionStatsRead, AgentReadPermission: authz.PermissionStatsRead},
	"getVisualAttachmentStatus":          {Permission: authz.PermissionSystemRead},
	"initCLIArchive":                     {Permission: authz.PermissionArchiveAdmin},
	"issueAgentToken":                    {Permission: authz.PermissionGrantManage},
	"listAccounts":                       {Permission: authz.PermissionSystemRead},
	"listAgentTokens":                    {Permission: authz.PermissionGrantManage},
	"listCLIAccounts":                    {Permission: authz.PermissionCLIRead, AgentReadPermission: authz.PermissionSearchRead, RemoteRead: true},
	"listCLICollections":                 {Permission: authz.PermissionCLIRead, AgentReadPermission: authz.PermissionSearchRead, RemoteRead: true},
	"listChangedMessages":                {Permission: authz.PermissionMessageRead},
	"listDeletions":                      {Permission: authz.PermissionDeletionStage},
	"listIdentityMatchCandidates":        {Permission: authz.PermissionPersonRead},
	"listMessageRecordings":              {Permission: authz.PermissionMessageRead},
	"listMessages":                       {Permission: authz.PermissionMessageRead, AgentReadPermission: authz.PermissionSearchRead},
	"listOperationRuns":                  {Permission: authz.PermissionSystemRead},
	"listSourceIdentities":               {Permission: authz.PermissionSystemRead},
	"listSourceStatus":                   {Permission: authz.PermissionSystemRead},
	"listTextConversationMessages":       {Permission: authz.PermissionMessageRead},
	"listTextConversations":              {Permission: authz.PermissionMessageRead},
	"planCLIAddCalendar":                 {Permission: authz.PermissionArchiveAdmin},
	"planCLIDeleteStaged":                {Permission: authz.PermissionDeletionStage},
	"planCLIEmbeddings":                  {Permission: authz.PermissionArchiveAdmin},
	"rebuildCLIFTS":                      {Permission: authz.PermissionArchiveAdmin},
	"repairEncodingCLI":                  {Permission: authz.PermissionArchiveAdmin},
	"repairMessageCLI":                   {Permission: authz.PermissionArchiveAdmin},
	"resumeVisualAttachmentBuild":        {Permission: authz.PermissionArchiveAdmin},
	"retireVisualAttachmentGeneration":   {Permission: authz.PermissionArchiveAdmin},
	"retryVisualAttachmentOwner":         {Permission: authz.PermissionArchiveAdmin},
	"reviewAcceptIdentityMatchCandidate": {Permission: authz.PermissionPersonMerge},
	"reviewRejectIdentityMatchCandidate": {Permission: authz.PermissionPersonMerge},
	"revokeAgentToken":                   {Permission: authz.PermissionGrantManage},
	"runArchiveQuery":                    {Permission: authz.PermissionSQLRead},
	"runCLI":                             {Permission: authz.PermissionArchiveAdmin, Delegated: true},
	"runQuery":                           {Permission: authz.PermissionSQLRead},
	"searchCLI":                          {Permission: authz.PermissionSearchRead, AgentReadPermission: authz.PermissionSearchRead, RemoteRead: true},
	"searchDocuments":                    {Permission: authz.PermissionSearchRead},
	"searchMedia":                        {Permission: authz.PermissionSearchRead},
	"searchMessages":                     {Permission: authz.PermissionSearchRead, AgentReadPermission: authz.PermissionSearchRead},
	"searchMessagesByDomains":            {Permission: authz.PermissionSearchRead, AgentReadPermission: authz.PermissionSearchRead},
	"searchTextMessages":                 {Permission: authz.PermissionSearchRead},
	"searchVisualAttachments":            {Permission: authz.PermissionSearchRead},
	"stageDeletion":                      {Permission: authz.PermissionDeletionStage},
	"startVisualAttachmentBuild":         {Permission: authz.PermissionArchiveAdmin},
	"syncCLI":                            {Permission: authz.PermissionArchiveAdmin},
	"syncFullCLI":                        {Permission: authz.PermissionArchiveAdmin},
	"triggerSync":                        {Permission: authz.PermissionArchiveAdmin},
	"uploadToken":                        {Permission: authz.PermissionArchiveAdmin},
	"verifyCLI":                          {Permission: authz.PermissionArchiveAdmin},

	// saved_views.go
	"createSavedView": {Permission: authz.PermissionCurationWrite},
	"deleteSavedView": {Permission: authz.PermissionCurationWrite},
	"getSavedView":    {Permission: authz.PermissionSearchRead},
	"listSavedViews":  {Permission: authz.PermissionSearchRead},
	"patchSavedView":  {Permission: authz.PermissionCurationWrite},

	// saved_views_run.go
	"runSavedView": {Permission: authz.PermissionSearchRead},

	// search_coverage.go
	"getSearchCoverage": {Permission: authz.PermissionSearchRead},

	// settings.go
	"getSettings":   {Permission: authz.PermissionArchiveAdmin},
	"patchSettings": {Permission: authz.PermissionArchiveAdmin},

	// settings_credentials.go
	"deleteSettingsProviderCredential": {Permission: authz.PermissionArchiveAdmin},
	"putSettingsProviderCredential":    {Permission: authz.PermissionArchiveAdmin},

	// settings_enrichment.go
	"putSettingsPersonEnrichmentProvider": {Permission: authz.PermissionArchiveAdmin},

	// settings_people_inference.go
	"cancelSettingsPeopleCodexLogin":         {Permission: authz.PermissionArchiveAdmin},
	"checkSettingsPeopleInferenceProvider":   {Permission: authz.PermissionArchiveAdmin},
	"consentSettingsPeopleInferenceProvider": {Permission: authz.PermissionArchiveAdmin},
	"deleteSettingsPeopleInferenceKey":       {Permission: authz.PermissionArchiveAdmin},
	"deleteSettingsPeopleInferenceProvider":  {Permission: authz.PermissionArchiveAdmin},
	"disableSettingsPeopleInference":         {Permission: authz.PermissionArchiveAdmin},
	"getSettingsPeopleCodexLogin":            {Permission: authz.PermissionArchiveAdmin},
	"getSettingsPeopleCodexModels":           {Permission: authz.PermissionArchiveAdmin},
	"getSettingsPeopleInference":             {Permission: authz.PermissionArchiveAdmin},
	"putSettingsPeopleCodexProfile":          {Permission: authz.PermissionArchiveAdmin},
	"putSettingsPeopleInferenceKey":          {Permission: authz.PermissionArchiveAdmin},
	"putSettingsPeopleInferencePreset":       {Permission: authz.PermissionArchiveAdmin},
	"revokeSettingsPeopleInferenceProvider":  {Permission: authz.PermissionArchiveAdmin},
	"selectSettingsPeopleInference":          {Permission: authz.PermissionArchiveAdmin},
	"startSettingsPeopleCodexLogin":          {Permission: authz.PermissionArchiveAdmin},

	// task_integration.go
	"getTaskIntegrationStatus": {Permission: authz.PermissionArchiveAdmin},
	"testTaskIntegration":      {Permission: authz.PermissionArchiveAdmin},

	// task_links.go
	"createOrLinkMessageTask": {Permission: authz.PermissionCurationWrite},
	"listMessageTasks":        {Permission: authz.PermissionSystemRead},
	"searchIntegrationTasks":  {Permission: authz.PermissionSystemRead},
	"unlinkMessageTask":       {Permission: authz.PermissionCurationWrite},

	// telemetry.go
	"captureTelemetryEvent": {Permission: authz.PermissionCurationWrite},
}
