package query

func deletionTargetSourceMessageIDs(targets []DeletionTarget, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(targets))
	for i := range targets {
		ids[i] = targets[i].SourceMessageID
	}
	return ids, nil
}
