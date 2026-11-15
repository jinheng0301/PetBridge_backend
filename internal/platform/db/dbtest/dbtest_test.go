package dbtest

import "testing"

func TestEnsureTestDB(t *testing.T) {
	ok := []string{
		"postgres://u:p@localhost:5432/petbridge_test?sslmode=disable",
		"postgres://u:p@localhost/other_test",
	}
	bad := []string{
		"postgres://u:p@localhost:5432/petbridge?sslmode=disable",
		"postgres://u:p@localhost:5432/petbridge_test_backup",
		"postgres://u:p@localhost:5432/",
		"postgres://u:p@localhost:5432",
		"://not a url",
	}
	for _, u := range ok {
		if err := ensureTestDB(u); err != nil {
			t.Errorf("%s rejected: %v", u, err)
		}
	}
	for _, u := range bad {
		if err := ensureTestDB(u); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
}
