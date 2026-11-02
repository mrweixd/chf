package rating

import (
	"testing"

	"github.com/fiorix/go-diameter/diam"
	"github.com/fiorix/go-diameter/diam/dict"
	"github.com/stretchr/testify/require"
)

func TestRejectUnsuccessfulOrMissingResult(t *testing.T) {
	m := diam.NewRequest(272, 4, dict.Default)
	for _, answer := range []*diam.Message{nil, m, m.Answer(diam.UnableToComply), m.Answer(diam.Success)} {
		r, err := decodeSUA(answer)
		require.Error(t, err)
		require.Nil(t, r)
	}
}
