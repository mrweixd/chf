package processor

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fiorix/go-diameter/diam/datatype"
	"github.com/gin-gonic/gin"
	"golang.org/x/exp/constraints"

	charging_datatype "github.com/free5gc/chf/ccs_diameter/datatype"
	"github.com/free5gc/chf/cdr/asn"
	"github.com/free5gc/chf/cdr/cdrConvert"
	"github.com/free5gc/chf/cdr/cdrType"
	"github.com/free5gc/chf/internal/abmf"
	"github.com/free5gc/chf/internal/cgf"
	chf_context "github.com/free5gc/chf/internal/context"
	"github.com/free5gc/chf/internal/logger"
	"github.com/free5gc/chf/internal/rating"
	"github.com/free5gc/chf/internal/tariff"
	"github.com/free5gc/chf/internal/util"
	Nchf_ConvergedCharging "github.com/free5gc/openapi/chf/ConvCharging"
	"github.com/free5gc/openapi/models"
	"github.com/free5gc/util/metrics/sbi"
)

func min[T constraints.Ordered](a, b T) T {
	if a < b {
		return a
	}
	return b
}

func (p *Processor) NotifyRecharge(ueId string, rg int32) {
	var reauthorizationDetails []models.Chf_ConvCharging_ReauthorizationDetails

	self := chf_context.GetSelf()
	ue, ok := self.ChfUeFindBySupi(ueId)
	if !ok {
		logger.NotifyEventLog.Errorf("Do not find charging data for UE: %s", ueId)
		return
	}

	// If it is previosly set to debit mode due to quota exhausted, need to reverse to the reserve mode
	ue.RatingType[rg] = charging_datatype.REQ_SUBTYPE_RESERVE
	reauthorizationDetails = append(reauthorizationDetails, models.Chf_ConvCharging_ReauthorizationDetails{
		RatingGroup: rg,
	})

	notifyRequest := models.Chf_ConvCharging_ChargingNotifyRequest{
		NotificationType:       models.Chf_ConvCharging_NotificationType_REAUTHORIZATION,
		ReauthorizationDetails: reauthorizationDetails,
	}

	p.SendChargingNotification(ue.NotifyUri, notifyRequest)
}

func (p *Processor) SendChargingNotification(
	notifyUri string, notifyRequest models.Chf_ConvCharging_ChargingNotifyRequest,
) {
	client := util.GetNchfChargingNotificationCallbackClient()
	logger.NotifyEventLog.Warn("Send Charging Notification  to SMF: uri: ", notifyUri)
	chargingNotifyRequest := &Nchf_ConvergedCharging.NullChargingNotificationRequest{
		RequestBody: &notifyRequest,
	}

	ctx, pd, err := chf_context.GetSelf().GetTokenCtx(
		models.Nrf_NFMgmt_ServiceName("nsmf-callback"), models.Nrf_NFMgmt_NFType_SMF)
	if err != nil {
		logger.NotifyEventLog.Warnf("SendChargingNotification get token failed: %+v", pd)
		return
	}

	_, err = client.DefaultApi.NullChargingNotification(ctx, notifyUri, chargingNotifyRequest)
	if err != nil {
		logger.NotifyEventLog.Warnf("Charging Notification Failed[%s]", err.Error())
		return
	}

	logger.NotifyEventLog.Tracef("Charging Notification Success")
}

func (p *Processor) HandleChargingdataInitial(
	c *gin.Context,
	chargingdata models.Chf_ConvCharging_ChargingDataRequest,
) {
	logger.ChargingdataPostLog.Infof("HandleChargingdataInitial")

	response, locationURI, problemDetails := p.ChargingDataCreate(chargingdata)

	if response != nil {
		c.Header("Location", locationURI)
		c.JSON(http.StatusCreated, response)
		return
	} else if problemDetails != nil {
		c.Set(sbi.IN_PB_DETAILS_CTX_STR, problemDetails.Cause)
		c.JSON(int(problemDetails.Status), problemDetails)
		return
	}
	problemDetails = &models.ProblemDetails{
		Status: http.StatusForbidden,
		Cause:  "UNSPECIFIED",
	}
	c.Set(sbi.IN_PB_DETAILS_CTX_STR, problemDetails.Cause)
	c.JSON(int(problemDetails.Status), problemDetails)
}

func (p *Processor) HandleChargingdataUpdate(
	c *gin.Context,
	chargingdata models.Chf_ConvCharging_ChargingDataRequest,
	chargingSessionId string,
) {
	logger.ChargingdataPostLog.Infof("HandleChargingdataUpdate")

	response, problemDetails := p.ChargingDataUpdate(chargingdata, chargingSessionId)

	if response != nil {
		c.JSON(http.StatusOK, response)
		return
	} else if problemDetails != nil {
		c.Set(sbi.IN_PB_DETAILS_CTX_STR, problemDetails.Cause)
		c.JSON(int(problemDetails.Status), problemDetails)
		return
	}
	problemDetails = &models.ProblemDetails{
		Status: http.StatusForbidden,
		Cause:  "UNSPECIFIED",
	}
	c.Set(sbi.IN_PB_DETAILS_CTX_STR, problemDetails.Cause)
	c.JSON(int(problemDetails.Status), problemDetails)
}

func (p *Processor) HandleChargingdataRelease(
	c *gin.Context,
	chargingdata models.Chf_ConvCharging_ChargingDataRequest,
	chargingSessionId string,
) {
	logger.ChargingdataPostLog.Infof("HandleChargingdateRelease")

	problemDetails := p.ChargingDataRelease(chargingdata, chargingSessionId)
	if problemDetails == nil {
		c.Status(http.StatusNoContent)
		return
	}
	c.Set(sbi.IN_PB_DETAILS_CTX_STR, problemDetails.Cause)
	c.JSON(int(problemDetails.Status), problemDetails)
}

func (p *Processor) ChargingDataCreate(
	chargingData models.Chf_ConvCharging_ChargingDataRequest,
) (
	*models.Chf_ConvCharging_ChargingDataResponse,
	string, *models.ProblemDetails,
) {
	var responseBody models.Chf_ConvCharging_ChargingDataResponse
	var chargingSessionId string

	if problemDetails := util.ValidateChargingDataCreateRequest(chargingData); problemDetails != nil {
		return nil, "", problemDetails
	}

	self := chf_context.GetSelf()
	ueId := chargingData.SubscriberIdentifier

	// Open CDR
	// ChargingDataRef(charging session id):
	// A unique identifier for a charging data resource in a PLMN
	// TODO determine charging session id(string type) supi+consumerid+localseq?
	ue, err := self.NewCHFUe(ueId)
	if err != nil {
		logger.ChargingdataPostLog.Errorf("New CHFUe error %s", err)
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return nil, "", problemDetails
	}

	ue.CULock.Lock()
	defer ue.CULock.Unlock()
	ue.NotifyUri = chargingData.NotifyUri

	consumerId := chargingData.NfConsumerIdentification.NFName
	if !chargingData.OneTimeEvent {
		chargingSessionId = ueId + consumerId + strconv.Itoa(int(self.LocalRecordSequenceNumber))
	}
	cdr, err := p.OpenCDR(chargingData, ue, chargingSessionId, false)
	if err != nil {
		logger.ChargingdataPostLog.Errorf("OpenCDR failed: %v", err)
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return nil, "", problemDetails
	}

	err = p.UpdateCDR(cdr, chargingData)
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return nil, "", problemDetails
	}

	ue.Cdr[chargingSessionId] = cdr
	ue.Records = append(ue.Records, ue.Cdr[chargingSessionId])

	if chargingData.OneTimeEvent {
		err = p.CloseCDR(cdr, false)
		if err != nil {
			problemDetails := &models.ProblemDetails{
				Status: http.StatusBadRequest,
			}
			return nil, "", problemDetails
		}
	}

	// CDR Transfer
	err = cgf.SendCDR(chargingData.SubscriberIdentifier)
	if err != nil {
		logger.ChargingdataPostLog.Errorf("Charging gateway fail to send CDR to billing domain %v", err)
	}

	logger.ChargingdataPostLog.Infof("Open CDR for UE %s", ueId)

	// build response
	logger.ChargingdataPostLog.Infof("NewChfUe %s", ueId)
	locationURI := self.Url + "/nchf-convergedcharging/v3/chargingdata/" + chargingSessionId
	timeStamp := time.Now()

	responseBody.InvocationTimeStamp = &timeStamp
	responseBody.InvocationSequenceNumber = chargingData.InvocationSequenceNumber

	return &responseBody, locationURI, nil
}

func (p *Processor) ChargingDataUpdate(
	chargingData models.Chf_ConvCharging_ChargingDataRequest, chargingSessionId string,
) (*models.Chf_ConvCharging_ChargingDataResponse, *models.ProblemDetails) {
	self := chf_context.GetSelf()
	ueId := chargingData.SubscriberIdentifier
	ue, ok := self.ChfUeFindBySupi(ueId)
	if !ok {
		logger.ChargingdataPostLog.Errorf("CHFUe[%s] not found", ueId)
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return nil, problemDetails
	}

	ue.CULock.Lock()
	defer ue.CULock.Unlock()

	cdr, ok := ue.Cdr[chargingSessionId]
	if !ok || cdr == nil {
		detail := fmt.Sprintf("ChargingDataRef[%s] not found", chargingSessionId)
		logger.ChargingdataPostLog.Warnln(detail)
		problemDetails := &models.ProblemDetails{
			Title:  "Context Not Found",
			Status: http.StatusNotFound,
			Detail: detail,
			Cause:  "CONTEXT_NOT_FOUND",
		}
		return nil, problemDetails
	}

	// Online charging: Rate, Account, Reservation
	responseBody, partialRecord := p.BuildConvergedChargingDataUpdateResopone(chargingData)

	cdrBytes, errCdrBer := asn.BerMarshalWithParams(&cdr, "explicit,choice")
	if errCdrBer != nil {
		logger.ChargingdataPostLog.Error(errCdrBer)
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
			Detail: errCdrBer.Error(),
		}
		return nil, problemDetails
	}

	var chgDataBytes []byte
	var errChgDataBer error
	if len(chargingData.MultipleUnitUsage) != 0 {
		cdrMultiUnitUsage := cdrConvert.MultiUnitUsageToCdr(chargingData.MultipleUnitUsage)
		chgDataBytes, errChgDataBer = asn.BerMarshalWithParams(&cdrMultiUnitUsage, "explicit,choice")
		if errChgDataBer != nil {
			logger.ChargingdataPostLog.Error(errChgDataBer)
			problemDetails := &models.ProblemDetails{
				Status: http.StatusBadRequest,
				Detail: errChgDataBer.Error(),
			}
			return nil, problemDetails
		}
	}

	if len(cdrBytes)+len(chgDataBytes) > math.MaxUint16 {
		var newRecord *cdrType.CHFRecord
		cdrJson, err := json.Marshal(cdr)
		if err != nil {
			logger.ChargingdataPostLog.Error(err)
		}
		err = json.Unmarshal(cdrJson, &newRecord)
		if err != nil {
			logger.ChargingdataPostLog.Error(err)
		}

		newRecord.ChargingFunctionRecord.ListOfMultipleUnitUsage = []cdrType.MultipleUnitUsage{}
		cdr = newRecord
		ue.Cdr[chargingSessionId] = cdr
		ue.Records = append(ue.Records, cdr)
	}

	err := p.UpdateCDR(cdr, chargingData)
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return nil, problemDetails
	}

	if partialRecord {
		ueId = chargingData.SubscriberIdentifier

		close_err := p.CloseCDR(cdr, partialRecord)
		if close_err != nil {
			logger.ChargingdataPostLog.Error("CloseCDR error:", close_err)
		}
		err = dumpCdrFile(ueId, []*cdrType.CHFRecord{cdr})
		if err != nil {
			problemDetails := &models.ProblemDetails{
				Status: http.StatusBadRequest,
			}
			return nil, problemDetails
		}

		reopenedCdr, oper_err := p.OpenCDR(chargingData, ue, chargingSessionId, partialRecord)
		if oper_err != nil {
			logger.ChargingdataPostLog.Error("OpenCDR error:", oper_err)
			problemDetails := &models.ProblemDetails{
				Status: http.StatusBadRequest,
				Detail: oper_err.Error(),
			}
			return nil, problemDetails
		}
		ue.Cdr[chargingSessionId] = reopenedCdr
		logger.ChargingdataPostLog.Tracef(
			"CDR Record Sequence Number after Reopen %+v", *cdr.ChargingFunctionRecord.RecordSequenceNumber)
	}

	err = dumpCdrFile(ueId, ue.Records)
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return nil, problemDetails
	}

	err = cgf.SendCDR(chargingData.SubscriberIdentifier)
	if err != nil {
		logger.ChargingdataPostLog.Errorf("Charging gateway fail to send CDR to billing domain %v", err)
	}

	timeStamp := time.Now()
	responseBody.InvocationTimeStamp = &timeStamp
	responseBody.InvocationSequenceNumber = chargingData.InvocationSequenceNumber

	return &responseBody, nil
}

func (p *Processor) ChargingDataRelease(
	chargingData models.Chf_ConvCharging_ChargingDataRequest, chargingSessionId string,
) *models.ProblemDetails {
	self := chf_context.GetSelf()
	ueId := chargingData.SubscriberIdentifier
	ue, ok := self.ChfUeFindBySupi(ueId)
	if !ok {
		logger.ChargingdataPostLog.Errorf("Do not find CHFUe[%s] error", ueId)
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return problemDetails
	}

	ue.CULock.Lock()
	defer ue.CULock.Unlock()

	cdr := ue.Cdr[chargingSessionId]
	if cdr == nil {
		return &models.ProblemDetails{Status: http.StatusNotFound, Cause: "CONTEXT_NOT_FOUND"}
	}
	progress, problem := settleRelease(ue, chargingSessionId, chargingData,
		func(
			entry models.Chf_ConvCharging_ChargingDataRequest, index int,
		) ([]models.Chf_ConvCharging_MultipleUnitInformation, bool) {
			return reserveEntry(entry, rating.SendServiceUsageRequest, abmf.SendAccountDebitRequest, chargingSessionId, index)
		})
	if problem != nil || progress.Finished {
		return problem
	}

	err := p.UpdateCDR(cdr, chargingData)
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
			Cause:  err.Error(),
		}
		return problemDetails
	}

	err = p.CloseCDR(cdr, false)
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return problemDetails
	}

	err = dumpCdrFile(ueId, []*cdrType.CHFRecord{cdr})
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return problemDetails
	}

	progress.Finished = true
	return nil
}

// Called under CULock. Advance only after this entry has been settled.
// Keep the cursor through CDR finalization so retrying cannot repeat a debit.
func settleRelease(ue *chf_context.ChfUe, session string, req models.Chf_ConvCharging_ChargingDataRequest,
	settle func(
		models.Chf_ConvCharging_ChargingDataRequest, int,
	) ([]models.Chf_ConvCharging_MultipleUnitInformation, bool),
) (*chf_context.ReleaseProgress, *models.ProblemDetails) {
	key, err := json.Marshal([]any{req.InvocationSequenceNumber, req.MultipleUnitUsage, req.Triggers})
	if err != nil {
		return nil, &models.ProblemDetails{Status: http.StatusBadRequest}
	}
	digest := sha256.Sum256(key)
	if ue.Releases == nil {
		ue.Releases = make(map[string]*chf_context.ReleaseProgress)
	}
	p := ue.Releases[session]
	if p == nil {
		p = &chf_context.ReleaseProgress{Fingerprint: digest}
		ue.Releases[session] = p
	}
	if p.Fingerprint != digest {
		return nil, &models.ProblemDetails{Status: http.StatusConflict, Cause: "CONFLICTING_REQUEST"}
	}
	entries := req.MultipleUnitUsage
	for p.Next < len(entries) {
		req.MultipleUnitUsage = entries[p.Next : p.Next+1]
		units, _ := settle(req, p.Next)
		for _, u := range units {
			if u.ResultCode == models.Chf_ConvCharging_ResultCode_RATING_FAILED {
				return nil, &models.ProblemDetails{Status: http.StatusInternalServerError, Cause: "RATING_FAILED"}
			}
		}
		p.Next++
	}
	return p, nil
}

func (p *Processor) BuildOnlineChargingDataCreateResopone(
	ue *chf_context.ChfUe, chargingData models.Chf_ConvCharging_ChargingDataRequest,
) models.Chf_ConvCharging_ChargingDataResponse {
	logger.ChargingdataPostLog.Info("In Build Online Charging Data Create Resopone")
	ue.NotifyUri = chargingData.NotifyUri

	multipleUnitInformation, _ := sessionChargingReservation(chargingData)

	responseBody := models.Chf_ConvCharging_ChargingDataResponse{
		MultipleUnitInformation: multipleUnitInformation,
	}

	return responseBody
}

func (p *Processor) BuildConvergedChargingDataUpdateResopone(
	chargingData models.Chf_ConvCharging_ChargingDataRequest,
) (models.Chf_ConvCharging_ChargingDataResponse, bool) {
	var partialRecord bool

	logger.ChargingdataPostLog.Info("In BuildConvergedChargingDataUpdateResopone")

	multipleUnitInformation, partialRecord := sessionChargingReservation(chargingData)

	responseBody := models.Chf_ConvCharging_ChargingDataResponse{
		MultipleUnitInformation: multipleUnitInformation,
	}

	return responseBody, partialRecord
}

// 32.296 6.2.2.3.1: Service usage request method with reservation
func sessionChargingReservation(
	chargingData models.Chf_ConvCharging_ChargingDataRequest,
) ([]models.Chf_ConvCharging_MultipleUnitInformation, bool) {
	return reserveWithPeers(chargingData, rating.SendServiceUsageRequest, abmf.SendAccountDebitRequest)
}

type (
	ratePeer func(
		*chf_context.ChfUe, *charging_datatype.ServiceUsageRequest,
	) (*charging_datatype.ServiceUsageResponse, error)
	accountPeer func(
		*chf_context.ChfUe, *charging_datatype.AccountDebitRequest,
	) (*charging_datatype.AccountDebitResponse, error)
)

func reserveWithPeers(chargingData models.Chf_ConvCharging_ChargingDataRequest, rate ratePeer, debit accountPeer,
) ([]models.Chf_ConvCharging_MultipleUnitInformation, bool) {
	return reserveEntry(chargingData, rate, debit, "", -1)
}

func reserveEntry(chargingData models.Chf_ConvCharging_ChargingDataRequest, rate ratePeer, debit accountPeer,
	session string, index int,
) ([]models.Chf_ConvCharging_MultipleUnitInformation, bool) {
	var multipleUnitInformation []models.Chf_ConvCharging_MultipleUnitInformation
	var partialRecord bool
	var subscriberIdentifier *charging_datatype.SubscriptionId

	self := chf_context.GetSelf()
	supi := chargingData.SubscriberIdentifier

	ue, ok := self.ChfUeFindBySupi(supi)
	if !ok {
		logger.ChargingdataPostLog.Warnf("Do not find UE[%s]", supi)
		return nil, false
	}

	supiType := strings.Split(supi, "-")[0]
	switch supiType {
	case "imsi":
		subscriberIdentifier = &charging_datatype.SubscriptionId{
			SubscriptionIdType: charging_datatype.END_USER_IMSI,
			SubscriptionIdData: datatype.UTF8String(supi[5:]),
		}
	case "nai":
		subscriberIdentifier = &charging_datatype.SubscriptionId{
			SubscriptionIdType: charging_datatype.END_USER_NAI,
			SubscriptionIdData: datatype.UTF8String(supi[4:]),
		}
	case "gci":
		subscriberIdentifier = &charging_datatype.SubscriptionId{
			SubscriptionIdType: charging_datatype.END_USER_NAI,
			SubscriptionIdData: datatype.UTF8String(supi[4:]),
		}
	case "gli":
		subscriberIdentifier = &charging_datatype.SubscriptionId{
			SubscriptionIdType: charging_datatype.END_USER_NAI,
			SubscriptionIdData: datatype.UTF8String(supi[4:]),
		}
	}

	for unitUsageNum, unitUsage := range chargingData.MultipleUnitUsage {
		var totalUsedUnit uint64
		invalidVolume := false
		var finalUnitIndication models.Chf_ConvCharging_FinalUnitIndication
		creditControl := false
		finalSeen := false

		rg := unitUsage.RatingGroup
		pending := ue.PendingRelease[rg]
		if pending != nil && (index < 0 || pending.Session != session ||
			int64(pending.Request.CcRequestNumber) != int64(index)) {
			// Another operation must not consume the reservation while settlement is unresolved.
			multipleUnitInformation = append(multipleUnitInformation, models.Chf_ConvCharging_MultipleUnitInformation{
				RatingGroup: rg, UPFID: unitUsage.UPFID, ResultCode: models.Chf_ConvCharging_ResultCode_RATING_FAILED,
			})
			continue
		}
		previousType, hadType := ue.RatingType[rg]
		previousGroups := ue.RatingGroups
		if !ue.FindRatingGroup(rg) {
			ue.RatingGroups = append(ue.RatingGroups, rg)
			ue.RatingType[rg] = charging_datatype.REQ_SUBTYPE_RESERVE
		}

		unitInformation := models.Chf_ConvCharging_MultipleUnitInformation{
			UPFID:               unitUsage.UPFID,
			FinalUnitIndication: &finalUnitIndication,
			RatingGroup:         rg,
		}

		for _, usedUnit := range unitUsage.UsedUnitContainer {
			switch usedUnit.QuotaManagementIndicator {
			case models.Chf_ConvCharging_QuotaManagementIndicator_OFFLINE_CHARGING:
				unitInformation.Triggers = append(unitInformation.Triggers,
					models.Chf_ConvCharging_Trigger{
						TriggerType:     models.Chf_ConvCharging_TriggerType_QUOTA_THRESHOLD,
						TriggerCategory: models.Chf_ConvCharging_TriggerCategory_IMMEDIATE_REPORT,
					},
				)

				unitInformation.VolumeQuotaThreshold = int32(30000000)
				continue
			case models.Chf_ConvCharging_QuotaManagementIndicator_ONLINE_CHARGING:
				creditControl = true

				for _, trigger := range chargingData.Triggers {
					// Check if partial record is needed
					partialRecord = true
					switch t := trigger; {
					case t == models.Chf_ConvCharging_Trigger{
						TriggerType:     models.Chf_ConvCharging_TriggerType_VOLUME_LIMIT,
						TriggerCategory: models.Chf_ConvCharging_TriggerCategory_IMMEDIATE_REPORT,
					}:
					case t.TriggerType == models.Chf_ConvCharging_TriggerType_MAX_NUMBER_OF_CHANGES_IN_CHARGING_CONDITIONS:
					case t.TriggerType == models.Chf_ConvCharging_TriggerType_MANAGEMENT_INTERVENTION:
					case t.TriggerType == models.Chf_ConvCharging_TriggerType_FINAL:
						finalSeen = true
						ue.RatingType[rg] = charging_datatype.REQ_SUBTYPE_DEBIT
						partialRecord = false
					}
				}
				// calculate total used unit
				if usedUnit.TotalVolume < 0 || totalUsedUnit > math.MaxInt64-uint64(usedUnit.TotalVolume) {
					invalidVolume = true
				} else {
					totalUsedUnit += uint64(usedUnit.TotalVolume)
				}
			case models.Chf_ConvCharging_QuotaManagementIndicator_QUOTA_MANAGEMENT_SUSPENDED:
				logger.ChargingdataPostLog.Errorf("Current do not support QUOTA MANAGEMENT SUSPENDED")
			}
		}
		if !creditControl {
			logger.ChargingdataPostLog.Infof("Credit Control are not required for rating group: %d", rg)
			continue
		}
		failRating := func(err error) {
			ue.RatingGroups = previousGroups
			if hadType {
				ue.RatingType[rg] = previousType
			} else {
				delete(ue.RatingType, rg)
			}
			logger.ChargingdataPostLog.Errorf("rating group %d: %v", rg, err)
			multipleUnitInformation = append(multipleUnitInformation, models.Chf_ConvCharging_MultipleUnitInformation{
				RatingGroup: rg, UPFID: unitUsage.UPFID, ResultCode: models.Chf_ConvCharging_ResultCode_RATING_FAILED,
			})
		}
		if invalidVolume || (unitUsage.RequestedUnit != nil && unitUsage.RequestedUnit.TotalVolume < 0) {
			failRating(fmt.Errorf("invalid charging volume"))
			continue
		}
		// TS 32.290 Table 7.1: ONLINE usage without Requested Unit is the last
		// request for this rating group, even without an explicit FINAL trigger.
		// Settle its usage and return unused reservations without granting new quota.
		if unitUsage.RequestedUnit == nil {
			ue.RatingType[rg] = charging_datatype.REQ_SUBTYPE_DEBIT
			partialRecord = false
		} else if finalSeen && unitUsage.RequestedUnit.TotalVolume > 0 {
			// A FINAL usage container can accompany a credit request for live services.
			ue.RatingType[rg] = charging_datatype.REQ_SUBTYPE_RESERVE
		}

		ccr := &charging_datatype.AccountDebitRequest{
			SessionId:       datatype.UTF8String(ue.AcctSessionId),
			OriginHost:      datatype.DiameterIdentity(self.AbmfCfg.OriginHost),
			OriginRealm:     datatype.DiameterIdentity(self.AbmfCfg.OriginRealm),
			EventTimestamp:  datatype.Time(time.Now()),
			SubscriptionId:  subscriberIdentifier,
			UserName:        datatype.OctetString(self.Name),
			CcRequestNumber: datatype.Unsigned32(ue.AcctRequestNum[rg]),
		}

		sur := &charging_datatype.ServiceUsageRequest{
			SessionId:      datatype.UTF8String(strconv.Itoa(int(ue.RateSessionId))),
			OriginHost:     datatype.DiameterIdentity(self.RatingCfg.OriginHost),
			OriginRealm:    datatype.DiameterIdentity(self.RatingCfg.OriginRealm),
			ActualTime:     datatype.Time(time.Now()),
			SubscriptionId: subscriberIdentifier,
			UserName:       datatype.OctetString(self.Name),
		}

		send := func(req *charging_datatype.ServiceUsageRequest) (*charging_datatype.ServiceUsageResponse, error) {
			return rate(ue, req)
		}
		quote := func() (tariff.Price, error) {
			if pending != nil {
				return pending.Price, nil
			}
			return getUnitCost(rg, sur, send)
		}
		unitsAt := func(price tariff.Price, units uint64, mode charging_datatype.RequestSubType) (int64, error) {
			if pending != nil {
				return pending.Units, nil
			}
			return ratedUnits(price, rg, sur, units, mode, send)
		}
		account := func(price tariff.Price, units int64) (*charging_datatype.AccountDebitResponse, error) {
			if index < 0 {
				return debit(ue, ccr)
			}
			if pending == nil {
				ccr.SessionId = datatype.UTF8String(ue.AcctSessionId + ";release;" + session)
				ccr.CcRequestNumber = datatype.Unsigned32(index)
				pending = &chf_context.PendingRelease{Session: session, Request: ccr, Price: price, Units: units}
				if ue.PendingRelease == nil {
					ue.PendingRelease = make(map[int32]*chf_context.PendingRelease)
				}
				ue.PendingRelease[rg] = pending
			}
			// Peer routing fields may be filled by the transport on each send.
			req := *pending.Request
			return debit(ue, &req)
		}
		switch ue.RatingType[rg] {
		case charging_datatype.REQ_SUBTYPE_RESERVE:
			price, err := quote()
			if err != nil {
				failRating(err)
				continue
			}
			requested := uint64(unitUsage.RequestedUnit.TotalVolume)
			usedQuota, remainder, err := usageCharge(price, totalUsedUnit, ue.UsageRemainder[rg], false)
			if err != nil {
				failRating(err)
				continue
			}
			if ue.ReservedQuota[rg] < math.MinInt64+usedQuota {
				failRating(fmt.Errorf("reservation underflow"))
				continue
			}
			balance := ue.ReservedQuota[rg] - usedQuota
			granted, err := unitsAt(price, requested, charging_datatype.REQ_SUBTYPE_RESERVE)
			if err != nil {
				failRating(err)
				continue
			}
			requestedQuota, _, err := usageCharge(price, uint64(granted), remainder, true)
			if err != nil {
				failRating(err)
				continue
			}
			if balance < requestedQuota {
				if balance < requestedQuota-math.MaxInt64 {
					failRating(fmt.Errorf("reservation exceeds account range"))
					continue
				}
				reserveQuota := requestedQuota - balance
				ccr.CcRequestType = charging_datatype.UPDATE_REQUEST
				ccr.RequestedAction = charging_datatype.DIRECT_DEBITING
				ccr.MultipleServicesCreditControl = &charging_datatype.MultipleServicesCreditControl{
					RatingGroup: datatype.Unsigned32(rg),
					RequestedServiceUnit: &charging_datatype.RequestedServiceUnit{
						CCTotalOctets: datatype.Unsigned64(reserveQuota),
					},
				}

				acctDebitRsp, debitErr := account(price, granted)
				if debitErr != nil {
					failRating(debitErr)
					continue
				}

				if acctDebitRsp == nil || acctDebitRsp.MultipleServicesCreditControl == nil ||
					acctDebitRsp.MultipleServicesCreditControl.GrantedServiceUnit == nil {
					failRating(fmt.Errorf("missing granted quota"))
					continue
				}
				actual := uint64(acctDebitRsp.MultipleServicesCreditControl.GrantedServiceUnit.CCTotalOctets)
				if actual > uint64(reserveQuota) {
					failRating(fmt.Errorf("invalid granted quota"))
					continue
				}
				balance += int64(actual)

				// Deduct the reserved quota from the account
				if acctDebitRsp.MultipleServicesCreditControl.FinalUnitIndication != nil {
					switch acctDebitRsp.MultipleServicesCreditControl.FinalUnitIndication.FinalUnitAction {
					case charging_datatype.TERMINATE:
						logger.ChargingdataPostLog.Tracef("Last granted quota")
						finalUnitIndication = models.Chf_ConvCharging_FinalUnitIndication{
							FinalUnitAction: models.Chf_ConvCharging_FinalUnitAction_TERMINATE,
						}
						ue.RatingType[rg] = charging_datatype.REQ_SUBTYPE_DEBIT
					}
				}
			}

			ue.ReservedQuota[rg] = balance
			ue.UsageRemainder[rg] = remainder
			var grantedUnit uint64
			if balance > 0 {
				grantedUnit = price.AffordableAfterUsage(uint64(balance), uint64(granted), remainder)
			}
			if ue.RatingType[rg] == charging_datatype.REQ_SUBTYPE_RESERVE {
				unitInformation.Triggers = append(unitInformation.Triggers,
					models.Chf_ConvCharging_Trigger{
						TriggerType:     models.Chf_ConvCharging_TriggerType_QUOTA_THRESHOLD,
						TriggerCategory: models.Chf_ConvCharging_TriggerCategory_IMMEDIATE_REPORT,
					},
				)

				unitInformation.VolumeQuotaThreshold = int32(float32(grantedUnit) * ue.VolumeThresholdRate)
			}

			unitInformation.Triggers = append(unitInformation.Triggers,
				models.Chf_ConvCharging_Trigger{
					TriggerType:     models.Chf_ConvCharging_TriggerType_QUOTA_EXHAUSTED,
					TriggerCategory: models.Chf_ConvCharging_TriggerCategory_IMMEDIATE_REPORT,
				},
			)

			unitInformation.GrantedUnit = &models.Chf_ConvCharging_GrantedUnit{
				TotalVolume:    int32(grantedUnit),
				DownlinkVolume: int32(grantedUnit),
				UplinkVolume:   int32(grantedUnit),
			}
			logger.ChargingdataPostLog.Tracef("granted Unit: %d", unitInformation.GrantedUnit.TotalVolume)

			// The timer of VolumeLimit is remain in SMF
			if ue.VolumeLimit != 0 {
				unitInformation.Triggers = append(unitInformation.Triggers,
					models.Chf_ConvCharging_Trigger{
						TriggerType:     models.Chf_ConvCharging_TriggerType_VOLUME_LIMIT,
						TriggerCategory: models.Chf_ConvCharging_TriggerCategory_DEFERRED_REPORT,
						VolumeLimit:     ue.VolumeLimit,
					},
				)
			}

			// VolumeLimit for PDU session only need to add once
			if ue.VolumeLimitPDU != 0 && unitUsageNum == 0 {
				unitInformation.Triggers = append(unitInformation.Triggers,
					models.Chf_ConvCharging_Trigger{
						TriggerType:     models.Chf_ConvCharging_TriggerType_VOLUME_LIMIT,
						TriggerCategory: models.Chf_ConvCharging_TriggerCategory_IMMEDIATE_REPORT,
						VolumeLimit:     ue.VolumeLimitPDU,
					},
				)
			}

			// The timer of QuotaValidityTime is remain in UPF
			if ue.QuotaValidityTime != 0 {
				unitInformation.Triggers = append(unitInformation.Triggers,
					models.Chf_ConvCharging_Trigger{
						TriggerType:     models.Chf_ConvCharging_TriggerType_VALIDITY_TIME,
						TriggerCategory: models.Chf_ConvCharging_TriggerCategory_IMMEDIATE_REPORT,
					},
				)
				unitInformation.ValidityTime = ue.QuotaValidityTime
			}

		case charging_datatype.REQ_SUBTYPE_DEBIT:
			logger.ChargingdataPostLog.Info("Debit mode, will not grant unit")
			price, err := quote()
			if err != nil {
				failRating(err)
				continue
			}
			finalPrice, err := unitsAt(price, totalUsedUnit, charging_datatype.REQ_SUBTYPE_DEBIT)
			if err != nil {
				failRating(err)
				continue
			}
			ratedPrice := finalPrice
			if carry := ue.UsageRemainder[rg]; carry != nil && carry.Sign() != 0 {
				quoted, e := price.Charge(totalUsedUnit)
				if e != nil || quoted != finalPrice {
					failRating(fmt.Errorf("cannot reconcile fractional usage with final rating"))
					continue
				}
				finalPrice, _, err = usageCharge(price, totalUsedUnit, carry, true)
				if err != nil {
					failRating(err)
					continue
				}
			}
			if ue.ReservedQuota[rg] < finalPrice-math.MaxInt64 {
				failRating(fmt.Errorf("settlement exceeds account range"))
				continue
			}
			if finalPrice < ue.ReservedQuota[rg] {
				// The final consumed quota is smaller than the reserved quota
				// Therefore, return the extra reserved quota back to the user account
				reservedRemained := ue.ReservedQuota[rg] - finalPrice
				ccr.RequestedAction = charging_datatype.REFUND_ACCOUNT
				ccr.MultipleServicesCreditControl = &charging_datatype.MultipleServicesCreditControl{
					RatingGroup: datatype.Unsigned32(rg),
					RequestedServiceUnit: &charging_datatype.RequestedServiceUnit{
						CCTotalOctets: datatype.Unsigned64(reservedRemained),
					},
				}
				// Typically, the reserved quota will be exhausted for the flow (or PDU session)
				// However, for the case the flow quota  and PDU session's quota is both last granted quota
				// and the PDU session's quota is larger than the flow's quota
				// PDU session's quota should be refund and set to reserved mode in order to reserve the quota for other flow
				ue.RatingType[rg] = charging_datatype.REQ_SUBTYPE_RESERVE
			} else {
				// The final consumed quota exceed the reserved quota
				// Deduct the extra consumed quota from the user account
				extraConsumed := finalPrice - ue.ReservedQuota[rg]
				ccr.RequestedAction = charging_datatype.DIRECT_DEBITING
				ccr.CcRequestType = charging_datatype.TERMINATION_REQUEST
				ccr.MultipleServicesCreditControl = &charging_datatype.MultipleServicesCreditControl{
					RatingGroup: datatype.Unsigned32(rg),
					UsedServiceUnit: &charging_datatype.UsedServiceUnit{
						CCTotalOctets: datatype.Unsigned64(extraConsumed),
					},
				}
			}

			_, err = account(price, ratedPrice)
			if err != nil {
				failRating(err)
				continue
			}
			ue.ReservedQuota[rg] = 0
			delete(ue.UsageRemainder, rg)

			unitInformation.Triggers = append(unitInformation.Triggers,
				models.Chf_ConvCharging_Trigger{
					TriggerType:     models.Chf_ConvCharging_TriggerType_QUOTA_EXHAUSTED,
					TriggerCategory: models.Chf_ConvCharging_TriggerCategory_IMMEDIATE_REPORT,
				},
			)
			unitInformation.GrantedUnit = &models.Chf_ConvCharging_GrantedUnit{
				TotalVolume:    int32(0),
				DownlinkVolume: int32(0),
				UplinkVolume:   int32(0),
			}
		}
		multipleUnitInformation = append(multipleUnitInformation, unitInformation)

		delete(ue.PendingRelease, rg)
		ue.AcctRequestNum[rg]++
	}

	return multipleUnitInformation, partialRecord
}
