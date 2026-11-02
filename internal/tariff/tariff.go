// Package tariff implements exact decimal prices for charging and rating.
package tariff

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Price is a positive number of account units per service unit.
type Price struct{ rate *big.Rat }

// New interprets Diameter Unit-Value as Value-Digits multiplied by 10^Exponent.
// Bound the scale before exponentiating untrusted Diameter input.
func New(digits int64, exp int32) (Price, error) {
	if digits <= 0 || exp < -38 || exp > 18 {
		return Price{}, fmt.Errorf("invalid or unsupported tariff %dE%d", digits, exp)
	}
	scale := int64(exp)
	if scale < 0 {
		scale = -scale
	}
	factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(scale), nil)
	value := new(big.Rat).SetInt64(digits)
	if exp < 0 {
		value.Quo(value, new(big.Rat).SetInt(factor))
	} else {
		value.Mul(value, new(big.Rat).SetInt(factor))
	}
	return Price{value}, nil
}

// Decimal converts a configured decimal (not floating point) to a wire pair.
func Decimal(s string) (int64, int32, error) {
	whole, frac, _ := strings.Cut(s, ".")
	if whole+frac == "" || strings.Trim(whole+frac, "0123456789") != "" {
		return 0, 0, fmt.Errorf("invalid decimal tariff")
	}
	frac = strings.TrimRight(frac, "0")
	if len(frac) > 38 {
		return 0, 0, fmt.Errorf("unsupported decimal scale")
	}
	digits, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	exp := -int32(len(frac))
	_, err = New(digits, exp)
	return digits, exp, err
}

func (p Price) Exact(units uint64) *big.Rat {
	return new(big.Rat).Mul(p.rate, new(big.Rat).SetInt(new(big.Int).SetUint64(units)))
}

// Charge rounds up only after multiplying the entire quantity.
func (p Price) Charge(units uint64) (int64, error) {
	x := p.Exact(units)
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(x.Num(), x.Denom(), r)
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("charge exceeds signed account range")
	}
	return q.Int64(), nil
}

// Affordable returns whole service units covered by budget, capped before conversion.
func (p Price) Affordable(budget, limit uint64) uint64 {
	return p.AffordableAfterUsage(budget, limit, nil)
}

// AffordableAfterUsage excludes outstanding fractional usage from the available balance.
func (p Price) AffordableAfterUsage(budget, limit uint64, owing *big.Rat) uint64 {
	x := new(big.Rat).SetInt(new(big.Int).SetUint64(budget))
	if owing != nil {
		x.Sub(x, owing)
	}
	if x.Sign() <= 0 {
		return 0
	}
	x.Quo(x, p.rate)
	n := new(big.Int).Quo(x.Num(), x.Denom())
	if !n.IsUint64() || n.Uint64() > limit {
		return limit
	}
	return n.Uint64()
}

// Chunk keeps volume and price representable in the existing Unsigned32 AVPs.
// Non-final chunks have integral prices so splitting introduces no extra rounding.
func (p Price) Chunk(units uint64) (uint64, error) {
	max := p.Affordable(math.MaxUint32, math.MaxUint32)
	if units <= max {
		return units, nil
	}
	d := p.rate.Denom()
	if !d.IsUint64() || d.Uint64() > max {
		return 0, fmt.Errorf("tariff cannot be split into representable Diameter quotas")
	}
	return max - max%d.Uint64(), nil
}
