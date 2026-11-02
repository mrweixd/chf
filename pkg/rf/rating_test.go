package rf

import (
	"math"
	"testing"

	"github.com/fiorix/go-diameter/diam/datatype"
	"github.com/stretchr/testify/require"

	cd "github.com/free5gc/chf/ccs_diameter/datatype"
)

func TestDecimalTariffExponent(t *testing.T) {
	tariff, err := buildTariff("0.05")
	if err != nil {
		t.Fatal(err)
	}
	v := tariff.RateElement.UnitCost
	if v.ValueDigits != 5 || v.Exponent != -2 {
		t.Fatalf("0.05 encoded as digits=%d exponent=%d", v.ValueDigits, v.Exponent)
	}
}

func TestRFPrecisionAndInvalidPrices(t *testing.T) {
	for _, tc := range []struct {
		raw          string
		budget, want uint32
	}{
		{"0.05", 1, 20}, {"0.0000000001", 1, math.MaxUint32}, {"1000", 4000000000, 4000000},
	} {
		r, err := rateUsage(tc.raw, &cd.ServiceRating{
			RequestSubType: cd.REQ_SUBTYPE_RESERVE, MonetaryQuota: datatype.Unsigned32(tc.budget),
		})
		require.NoError(t, err)
		require.EqualValues(t, tc.want, r.AllowedUnits)
	}
	for _, bad := range []string{"0", "-1", "n/a", "1.2.3"} {
		_, err := rateUsage(bad, &cd.ServiceRating{})
		require.Error(t, err)
	}
	_, err := rateUsage("1000", &cd.ServiceRating{RequestSubType: cd.REQ_SUBTYPE_DEBIT, ConsumedUnits: 5000000})
	require.Error(t, err)
}
