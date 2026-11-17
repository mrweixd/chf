package util

import (

	"github.com/gin-gonic/gin"

	chf_context "github.com/free5gc/chf/internal/context"
	"github.com/free5gc/chf/internal/logger"
	"github.com/free5gc/openapi/models"
)

type NFContextGetter func() *chf_context.CHFContext

type RouterAuthorizationCheck struct {
	serviceName models.Nrf_NFMgmt_ServiceName
}

func NewRouterAuthorizationCheck(serviceName models.Nrf_NFMgmt_ServiceName) *RouterAuthorizationCheck {
	return &RouterAuthorizationCheck{
		serviceName: serviceName,
	}
}

func (rac *RouterAuthorizationCheck) Check(c *gin.Context, chfContext chf_context.NFContext) {
	token := c.Request.Header.Get("Authorization")
	err := chfContext.AuthorizationCheck(token, rac.serviceName)
	if err != nil {
		logger.UtilLog.Debugf("RouterAuthorizationCheck::Check Unauthorized: %s", err.Error())
		status, challenge, reason := oauthChallenge(c.Request, string(rac.serviceName), token, err)
		c.Header("WWW-Authenticate", challenge)
		c.JSON(status, gin.H{"error": reason})
		c.Abort()
		return
	}

	logger.UtilLog.Debugf("RouterAuthorizationCheck::Check Authorized")
}
