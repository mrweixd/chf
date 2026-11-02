package processor

import (
	"fmt"
	"math"
	"math/big"

	"github.com/fiorix/go-diameter/diam/datatype"

	cd "github.com/free5gc/chf/ccs_diameter/datatype"
	"github.com/free5gc/chf/internal/tariff"
)

type ratingCall func(*cd.ServiceUsageRequest) (*cd.ServiceUsageResponse, error)

func getUnitCost(rg int32, sur *cd.ServiceUsageRequest, send ratingCall) (tariff.Price, error) {
	if sur == nil {
		return tariff.Price{}, fmt.Errorf("missing rating request")
	}
	sur.ServiceRating = &cd.ServiceRating{
		ServiceIdentifier: datatype.Unsigned32(rg), RequestSubType: cd.REQ_SUBTYPE_RESERVE,
	}
	rsp, err := send(sur)
	if err != nil {
		return tariff.Price{}, err
	}
	if rsp == nil || rsp.ServiceRating == nil || rsp.ServiceRating.MonetaryTariff == nil ||
		rsp.ServiceRating.MonetaryTariff.RateElement == nil || rsp.ServiceRating.MonetaryTariff.RateElement.UnitCost == nil {
		return tariff.Price{}, fmt.Errorf("rating answer omitted unit cost")
	}
	u := rsp.ServiceRating.MonetaryTariff.RateElement.UnitCost
	return tariff.New(int64(u.ValueDigits), int32(u.Exponent))
}

// Complete rating before any account operation. Bound the number of requests
// rather than turning an unrepresentable tariff into billions of exchanges.
func ratedUnits(p tariff.Price, rg int32, sur *cd.ServiceUsageRequest, units uint64,
	mode cd.RequestSubType, send ratingCall,
) (int64, error) {
	var result int64
	for i := 0; i == 0 || units > 0; i++ {
		if i >= 128 {
			return 0, fmt.Errorf("rating exceeds batch limit")
		}
		chunk, err := p.Chunk(units)
		if err != nil {
			return 0, err
		}
		req := &cd.ServiceRating{ServiceIdentifier: datatype.Unsigned32(rg), RequestSubType: mode}
		if i == 0 && units != 0 && (units-1)/chunk >= 128 {
			return 0, fmt.Errorf("rating exceeds batch limit")
		}
		if mode == cd.REQ_SUBTYPE_RESERVE {
			amount, e := p.Charge(chunk)
			if e != nil {
				return 0, e
			}
			req.MonetaryQuota = datatype.Unsigned32(amount)
			req.RequestedUnits = datatype.Unsigned32(chunk)
		} else {
			req.ConsumedUnits = datatype.Unsigned32(chunk)
		}
		sur.ServiceRating = req
		answer, e := send(sur)
		if e != nil {
			return 0, e
		}
		if answer == nil || answer.ServiceRating == nil {
			return 0, fmt.Errorf("empty rating answer")
		}
		n := int64(answer.ServiceRating.Price)
		if mode == cd.REQ_SUBTYPE_RESERVE {
			n = int64(min(uint64(answer.ServiceRating.AllowedUnits), chunk))
		}
		if result > math.MaxInt64-n {
			return 0, fmt.Errorf("rating total overflow")
		}
		result += n
		if mode == cd.REQ_SUBTYPE_RESERVE && uint64(n) < chunk {
			break
		}
		units -= chunk
	}
	return result, nil
}

// Carry fractional usage across interim reports; settle the remainder only on
// release. This makes twenty reports of one unit at 0.05 cost one, not twenty.
func usageCharge(p tariff.Price, units uint64, carry *big.Rat, final bool) (int64, *big.Rat, error) {
	x := p.Exact(units)
	if carry != nil {
		x.Add(x, carry)
	}
	whole := new(big.Int).Quo(x.Num(), x.Denom())
	remainder := new(big.Rat).Sub(x, new(big.Rat).SetInt(whole))
	if final && remainder.Sign() != 0 {
		whole.Add(whole, big.NewInt(1))
		remainder.SetInt64(0)
	}
	if !whole.IsInt64() {
		return 0, nil, fmt.Errorf("usage exceeds account range")
	}
	return whole.Int64(), remainder, nil
}
