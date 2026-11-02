package processor

import (
	"encoding/json"
	"errors"
	"math/big"
	"testing"

	"github.com/fiorix/go-diameter/diam/datatype"
	"github.com/fiorix/go-diameter/diam/sm"
	"github.com/stretchr/testify/require"

	cd "github.com/free5gc/chf/ccs_diameter/datatype"
	ctx "github.com/free5gc/chf/internal/context"
	"github.com/free5gc/chf/internal/tariff"
	"github.com/free5gc/openapi/models"
)

func TestReservationArithmetic(t *testing.T) {
	self := ctx.GetSelf()
	oldRF, oldAB := self.RatingCfg, self.AbmfCfg
	self.RatingCfg, self.AbmfCfg = &sm.Settings{}, &sm.Settings{}
	t.Cleanup(func() { self.RatingCfg, self.AbmfCfg = oldRF, oldAB })
	for _, tc := range []struct {
		name              string
		digits            int64
		exp, volume, used int32
		balance, want     int64
		failAt            int
	}{
		{"five_billion_requested", 1000, 0, 5_000_000, 0, 0, 5_000_000_000, 0},
		{"five_billion_used", 1000, 0, 1, 5_000_000, 0, 5_000_001_000, 0},
		{"fraction", 5, -2, 100, 0, 0, 5, 0},
		{"tariff_one", 1, 0, 5, 0, 0, 5, 0},
		{"lookup_timeout", 1, 0, 10, 3, 7, 0, 1},
		{"later_rating_failure", 1000, 0, 5_000_000, 3, 7, 0, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const rg = 1
			const supi = "imsi-test1211"
			ue := &ctx.ChfUe{
				Supi: supi, ReservedQuota: map[int32]int64{rg: tc.balance}, RatingType: map[int32]cd.RequestSubType{},
				AcctRequestNum: map[int32]uint32{}, UsageRemainder: map[int32]*big.Rat{},
			}
			self.UePool.Store(supi, ue)
			t.Cleanup(func() { self.UePool.Delete(supi) })
			request := models.Chf_ConvCharging_ChargingDataRequest{
				SubscriberIdentifier: supi,
				MultipleUnitUsage: []models.Chf_ConvCharging_MultipleUnitUsage{{
					RatingGroup:   rg,
					RequestedUnit: &models.Chf_ConvCharging_RequestedUnit{TotalVolume: tc.volume},
					UsedUnitContainer: []models.Chf_ConvCharging_UsedUnitContainer{{
						QuotaManagementIndicator: models.Chf_ConvCharging_QuotaManagementIndicator_ONLINE_CHARGING, TotalVolume: tc.used,
					}},
				}},
			}
			var debited int64
			calls, debits := 0, 0
			groups, _ := reserveWithPeers(request, func(
				_ *ctx.ChfUe, s *cd.ServiceUsageRequest,
			) (*cd.ServiceUsageResponse, error) {
				calls++
				if calls == tc.failAt {
					return nil, errors.New("rating unavailable")
				}
				sr := s.ServiceRating
				if calls > 1 && tc.digits == 1000 {
					require.EqualValues(t, uint64(sr.RequestedUnits)*1000, sr.MonetaryQuota)
				}
				return &cd.ServiceUsageResponse{ServiceRating: &cd.ServiceRating{
					AllowedUnits: sr.RequestedUnits,
					MonetaryTariff: &cd.MonetaryTariff{RateElement: &cd.RateElement{UnitCost: &cd.UnitCost{
						ValueDigits: datatype.Integer64(tc.digits), Exponent: datatype.Integer32(tc.exp),
					}}},
				}}, nil
			}, func(_ *ctx.ChfUe, r *cd.AccountDebitRequest) (*cd.AccountDebitResponse, error) {
				debits++
				q := r.MultipleServicesCreditControl.RequestedServiceUnit.CCTotalOctets
				debited += int64(q)
				return &cd.AccountDebitResponse{MultipleServicesCreditControl: &cd.MultipleServicesCreditControl{
					GrantedServiceUnit: &cd.GrantedServiceUnit{CCTotalOctets: q},
				}}, nil
			})
			require.Len(t, groups, 1)
			if tc.failAt != 0 {
				require.Equal(t, models.Chf_ConvCharging_ResultCode_RATING_FAILED, groups[0].ResultCode)
				require.Nil(t, groups[0].GrantedUnit)
				require.Zero(t, debits)
				require.Equal(t, tc.balance, ue.ReservedQuota[rg])
				return
			}
			require.NotNil(t, groups[0].GrantedUnit)
			require.EqualValues(t, tc.volume, groups[0].GrantedUnit.TotalVolume)
			require.Equal(t, tc.want, debited)
		})
	}
}

func TestFractionalUsageSettlement(t *testing.T) {
	price, err := tariff.New(5, -2)
	require.NoError(t, err)
	var carry *big.Rat
	for i := 1; i <= 20; i++ {
		var paid int64
		paid, carry, err = usageCharge(price, 1, carry, false)
		require.NoError(t, err)
		require.EqualValues(t, i/20, paid)
		require.EqualValues(t, 20-i%20, price.AffordableAfterUsage(1, 20, carry))
	}
	_, carry, err = usageCharge(price, 1, carry, false)
	require.NoError(t, err)
	paid, carry, err := usageCharge(price, 0, carry, true)
	require.NoError(t, err)
	require.EqualValues(t, 1, paid)
	require.Zero(t, carry.Sign())
}

func TestLargeFinalDebit(t *testing.T) {
	price, err := tariff.New(1, 0)
	require.NoError(t, err)
	calls := 0
	total, err := ratedUnits(price, 1, &cd.ServiceUsageRequest{}, 5_000_000_000, cd.REQ_SUBTYPE_DEBIT,
		func(req *cd.ServiceUsageRequest) (*cd.ServiceUsageResponse, error) {
			calls++
			return &cd.ServiceUsageResponse{ServiceRating: &cd.ServiceRating{Price: &req.ServiceRating.ConsumedUnits}}, nil
		})
	require.NoError(t, err)
	require.EqualValues(t, 5_000_000_000, total)
	require.Equal(t, 2, calls)
}

func TestReleaseRetry(t *testing.T) {
	self := ctx.GetSelf()
	oldRF, oldAB := self.RatingCfg, self.AbmfCfg
	self.RatingCfg, self.AbmfCfg = &sm.Settings{}, &sm.Settings{}
	defer func() { self.RatingCfg, self.AbmfCfg = oldRF, oldAB }()
	ue := &ctx.ChfUe{
		Supi:           "imsi-audit",
		ReservedQuota:  map[int32]int64{},
		RatingType:     map[int32]cd.RequestSubType{},
		AcctRequestNum: map[int32]uint32{}, UsageRemainder: map[int32]*big.Rat{},
	}
	self.UePool.Store(ue.Supi, ue)
	defer self.UePool.Delete(ue.Supi)
	req := models.Chf_ConvCharging_ChargingDataRequest{SubscriberIdentifier: ue.Supi}
	for _, rg := range []int32{1, 2} {
		req.MultipleUnitUsage = append(req.MultipleUnitUsage, models.Chf_ConvCharging_MultipleUnitUsage{
			RatingGroup: rg,
			UsedUnitContainer: []models.Chf_ConvCharging_UsedUnitContainer{{
				QuotaManagementIndicator: models.Chf_ConvCharging_QuotaManagementIndicator_ONLINE_CHARGING, TotalVolume: 10,
			}},
		})
	}
	paid := map[uint32]int{}
	for attempt := 1; attempt <= 2; attempt++ {
		_, problem := settleRelease(ue, "session", req, func(
			one models.Chf_ConvCharging_ChargingDataRequest, _ int,
		) ([]models.Chf_ConvCharging_MultipleUnitInformation, bool) {
			return reserveWithPeers(one, func(_ *ctx.ChfUe, r *cd.ServiceUsageRequest) (*cd.ServiceUsageResponse, error) {
				sr := r.ServiceRating
				if sr.ServiceIdentifier == 2 && attempt == 1 && sr.RequestSubType == cd.REQ_SUBTYPE_DEBIT {
					return &cd.ServiceUsageResponse{ServiceRating: &cd.ServiceRating{}}, nil
				}
				return &cd.ServiceUsageResponse{ServiceRating: &cd.ServiceRating{
					Price:          &sr.ConsumedUnits,
					MonetaryTariff: &cd.MonetaryTariff{RateElement: &cd.RateElement{UnitCost: &cd.UnitCost{ValueDigits: 1}}},
				}}, nil
			}, func(_ *ctx.ChfUe, r *cd.AccountDebitRequest) (*cd.AccountDebitResponse, error) {
				cc := r.MultipleServicesCreditControl
				paid[uint32(cc.RatingGroup)] += int(cc.UsedServiceUnit.CCTotalOctets)
				return &cd.AccountDebitResponse{}, nil
			})
		})
		if attempt == 1 {
			require.NotNil(t, problem)
			require.Zero(t, paid[2])
			require.Equal(t, 10, paid[1])
		} else {
			require.Nil(t, problem)
		}
	}
	require.Equal(t, 10, paid[uint32(2)])
	require.Equal(t, 10, paid[uint32(1)], "first group should be debited once")
}

func TestMissingDebitPrice(t *testing.T) {
	p, err := tariff.New(1, 0)
	require.NoError(t, err)
	_, err = ratedUnits(p, 1, &cd.ServiceUsageRequest{}, 10, cd.REQ_SUBTYPE_DEBIT,
		func(*cd.ServiceUsageRequest) (*cd.ServiceUsageResponse, error) {
			return &cd.ServiceUsageResponse{ServiceRating: &cd.ServiceRating{}}, nil
		})
	require.ErrorContains(t, err, "omitted Price")
}

func TestReleaseRetryAfterTariffChange(t *testing.T) {
	self := ctx.GetSelf()
	oldRF, oldAB := self.RatingCfg, self.AbmfCfg
	self.RatingCfg, self.AbmfCfg = &sm.Settings{}, &sm.Settings{}
	defer func() { self.RatingCfg, self.AbmfCfg = oldRF, oldAB }()
	for _, test := range []struct {
		name     string
		reserved int64
		want     int64
	}{
		{"debit", 0, 90}, {"refund", 20, 110},
	} {
		t.Run(test.name, func(t *testing.T) {
			const rg = 1
			u := &ctx.ChfUe{
				Supi: "imsi-replay", AcctSessionId: "acct",
				ReservedQuota: map[int32]int64{rg: test.reserved}, RatingType: map[int32]cd.RequestSubType{},
				AcctRequestNum: map[int32]uint32{}, UsageRemainder: map[int32]*big.Rat{},
			}
			self.UePool.Store(u.Supi, u)
			defer self.UePool.Delete(u.Supi)
			req := models.Chf_ConvCharging_ChargingDataRequest{
				SubscriberIdentifier: u.Supi,
				MultipleUnitUsage: []models.Chf_ConvCharging_MultipleUnitUsage{{
					RatingGroup: rg,
					UsedUnitContainer: []models.Chf_ConvCharging_UsedUnitContainer{{
						TotalVolume:              1,
						QuotaManagementIndicator: models.Chf_ConvCharging_QuotaManagementIndicator_ONLINE_CHARGING,
					}},
				}},
			}
			price := datatype.Unsigned32(10)
			rfCalls := 0
			rate := func(_ *ctx.ChfUe, _ *cd.ServiceUsageRequest) (*cd.ServiceUsageResponse, error) {
				rfCalls++
				p := price
				return &cd.ServiceUsageResponse{ServiceRating: &cd.ServiceRating{
					Price: &p,
					MonetaryTariff: &cd.MonetaryTariff{
						RateElement: &cd.RateElement{UnitCost: &cd.UnitCost{ValueDigits: datatype.Integer64(p)}},
					},
				}}, nil
			}
			var original []byte
			ledger := int64(100)
			sends := 0
			account := func(_ *ctx.ChfUe, r *cd.AccountDebitRequest) (*cd.AccountDebitResponse, error) {
				sends++
				wire, err := json.Marshal(r)
				require.NoError(t, err)
				if original != nil {
					require.Equal(t, original, wire)
					return &cd.AccountDebitResponse{}, nil
				}
				original = wire
				m := r.MultipleServicesCreditControl
				if r.RequestedAction == cd.REFUND_ACCOUNT {
					ledger += int64(m.RequestedServiceUnit.CCTotalOctets)
				} else {
					ledger -= int64(m.UsedServiceUnit.CCTotalOctets)
				}
				return nil, errors.New("committed, answer lost")
			}
			settle := func(
				entry models.Chf_ConvCharging_ChargingDataRequest, index int,
			) ([]models.Chf_ConvCharging_MultipleUnitInformation, bool) {
				return reserveEntry(entry, rate, account, "session", index)
			}
			_, problem := settleRelease(u, "session", req, settle)
			require.NotNil(t, problem)
			require.Equal(t, test.want, ledger)
			before := rfCalls
			price = 11
			other, _ := reserveEntry(req, rate, account, "other-session", 0)
			require.Equal(t, models.Chf_ConvCharging_ResultCode_RATING_FAILED, other[0].ResultCode)
			progress, problem := settleRelease(u, "session", req, settle)
			require.Nil(t, problem)
			require.Equal(t, 1, progress.Next)
			require.Equal(t, before, rfCalls)
			require.Equal(t, 2, sends)
			require.Equal(t, test.want, ledger)
			require.Empty(t, u.PendingRelease)
			require.Zero(t, u.ReservedQuota[rg])
		})
	}
}
