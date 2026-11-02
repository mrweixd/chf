package rating

import (
	"bytes"
	"strings"
	"testing"

	"github.com/fiorix/go-diameter/diam"
	"github.com/fiorix/go-diameter/diam/datatype"
	"github.com/fiorix/go-diameter/diam/dict"
	"github.com/stretchr/testify/require"

	cd "github.com/free5gc/chf/ccs_diameter/datatype"
	cfg "github.com/free5gc/chf/ccs_diameter/dict"
)

func TestRejectUnsuccessfulOrMissingResult(t *testing.T) {
	m := diam.NewRequest(272, 4, dict.Default)
	for _, answer := range []*diam.Message{nil, m, m.Answer(diam.UnableToComply), m.Answer(diam.Success)} {
		r, err := decodeSUA(answer)
		require.Error(t, err)
		require.Nil(t, r)
	}
}

func TestSuccessAnswerWire(t *testing.T) {
	d := dict.Default
	require.NoError(t, d.Load(strings.NewReader(cfg.RateDictionary)))
	zero := datatype.Unsigned32(0)
	for _, price := range []*datatype.Unsigned32{nil, &zero} {
		req := diam.NewRequest(111, 16777218, d)
		s := cd.ServiceUsageResponse{ServiceRating: &cd.ServiceRating{Price: price}}
		a, err := s.SuccessAnswer(req)
		require.NoError(t, err)
		var wire bytes.Buffer
		_, err = a.WriteTo(&wire)
		require.NoError(t, err)
		actual, err := diam.ReadMessage(&wire, d)
		require.NoError(t, err)
		decoded, err := decodeSUA(actual)
		require.NoError(t, err)
		require.Equal(t, price, decoded.ServiceRating.Price)
	}
}
