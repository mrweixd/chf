package datatype

import (
	"github.com/fiorix/go-diameter/diam"
	diam_datatype "github.com/fiorix/go-diameter/diam/datatype"
)

type ServiceUsageResponse struct {
	ResultCode          diam_datatype.Unsigned32       `avp:"Result-Code"`
	SessionId           diam_datatype.UTF8String       `avp:"Session-Id"`
	OriginHost          diam_datatype.DiameterIdentity `avp:"Origin-Host"`
	OriginRealm         diam_datatype.DiameterIdentity `avp:"Origin-Realm"`
	VendorSpecificAppId diam_datatype.Grouped          `avp:"Vendor-Specific-Application-Id"`
	EventTimestamp      diam_datatype.Time             `avp:"Event-Timestamp"`
	ServiceRating       *ServiceRating                 `avp:"Service-Rating"`
}

// SuccessAnswer marshals the result together with the rating: Marshal replaces
// all AVPs, including the Result-Code initially inserted by Answer.
func (s ServiceUsageResponse) SuccessAnswer(request *diam.Message) (*diam.Message, error) {
	a := request.Answer(diam.Success)
	s.ResultCode = diam.Success
	if err := a.Marshal(&s); err != nil {
		return nil, err
	}
	return a, nil
}
