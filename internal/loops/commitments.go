package loops

import (
	"errors"
	"strconv"
	"strings"
)

var ErrInvalidCommitmentID = errors.New("commitment ID must be c followed by a positive integer")

func FormatCommitmentID(id int64) string {
	return "c" + strconv.FormatInt(id, 10)
}

func ParseCommitmentID(value string) (int64, error) {
	number, found := strings.CutPrefix(value, "c")
	id, err := strconv.ParseInt(number, 10, 64)
	if !found || err != nil || id <= 0 || FormatCommitmentID(id) != value {
		return 0, ErrInvalidCommitmentID
	}
	return id, nil
}
