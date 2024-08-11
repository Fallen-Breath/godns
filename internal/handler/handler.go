package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TimothyYe/godns/internal/provider"

	log "github.com/sirupsen/logrus"

	"github.com/TimothyYe/godns/internal/settings"
	"github.com/TimothyYe/godns/internal/utils"
	"github.com/TimothyYe/godns/pkg/lib"
	"github.com/TimothyYe/godns/pkg/notification"
)

var (
	errEmptyResult = errors.New("empty result")
	errEmptyDomain = errors.New("NXDOMAIN")
)

type Handler struct {
	Configuration       *settings.Settings
	dnsProvider         provider.IDNSProvider
	notificationManager notification.INotificationManager
	cachedIP            string
	cacheTime           time.Time
}

func (handler *Handler) SetConfiguration(conf *settings.Settings) {
	handler.Configuration = conf
	handler.notificationManager = notification.GetNotificationManager(handler.Configuration)
}

func (handler *Handler) SetProvider(provider provider.IDNSProvider) {
	handler.dnsProvider = provider
}

func (handler *Handler) LoopUpdateIP(ctx context.Context, domains *[]settings.Domain) error {
	ticker := time.NewTicker(time.Second * time.Duration(handler.Configuration.Interval))

	// run once at the beginning
	err := handler.UpdateIP(domains)
	if err != nil {
		log.WithError(err).Debug("Update IP failed during the DNS Update loop")
	}
	log.Debugf("DNS update loop finished, will run again in %d seconds", handler.Configuration.Interval)

	for {
		select {
		case <-ticker.C:
			err := handler.UpdateIP(domains)
			if err != nil {
				log.WithError(err).Debug("Update IP failed during the DNS Update loop")
			}
			log.Debugf("DNS update loop finished, will run again in %d seconds", handler.Configuration.Interval)
		case <-ctx.Done():
			log.Info("DNS update loop cancelled")
			ticker.Stop()
			return nil
		}
	}
}

func (handler *Handler) UpdateIP(domains *[]settings.Domain) error {
	ip, err := utils.GetCurrentIP(handler.Configuration)
	if err != nil {
		if handler.Configuration.RunOnce {
			return fmt.Errorf("%v: fail to get current IP", err)
		}
		log.Error(err)
		return nil
	}

	if time.Now().Sub(handler.cacheTime) >= utils.DefaultIPCacheTimeout {
		if !handler.cacheTime.IsZero() {
			log.Debugf("cache IP (%s %s) expired", handler.cachedIP, handler.cacheTime.Format(time.DateTime))
		}
		handler.cachedIP = ""
		handler.cacheTime = time.Time{}
	}

	if ip == handler.cachedIP {
		log.Debugf("IP (%s) matches cached IP (%s), skipping", ip, handler.cachedIP)
		return nil
	}

	for _, domain := range *domains {
		err = handler.updateDNS(&domain, ip)
		if err != nil {
			if handler.Configuration.RunOnce {
				return fmt.Errorf("%v: fail to update DNS", err)
			}
			log.Error(err)
			return nil
		}
	}
	handler.cachedIP = ip
	handler.cacheTime = time.Now()
	log.Debugf("Cached IP address: %s", ip)
	return nil
}

func (handler *Handler) updateDNS(domain *settings.Domain, ip string) error {
	var updatedDomains []string
	for _, subdomainName := range domain.SubDomains {

		var hostname string
		if subdomainName != utils.RootDomain {
			hostname = subdomainName + "." + domain.DomainName
		} else {
			hostname = domain.DomainName
		}

		lastIP, err := utils.ResolveDNS(hostname, handler.Configuration.Resolver, handler.Configuration.IPType)
		if err != nil && (errors.Is(err, errEmptyResult) || errors.Is(err, errEmptyDomain)) {
			log.Errorf("Failed to resolve DNS for domain: %s, error: %s", hostname, err)
			continue
		}
		if err != nil {
			log.Warnf("Failed to resolve DNS for domain: %s, error: %s", hostname, err)
		}

		//check against the current known IP, if no change, skip update
		if ip == lastIP {
			log.Infof("IP is the same as the resolved one, skip update, domain: %s, current IP: %s, resolved IP: %s", hostname, ip, lastIP)
		} else {
			log.Infof("IP is different from the resolved one, do update, domain: %s, current IP: %s, resolved IP: %s", hostname, ip, lastIP)

			if err := handler.dnsProvider.UpdateIP(domain.DomainName, subdomainName, ip); err != nil {
				return err
			}

			updatedDomains = append(updatedDomains, subdomainName)

			// execute webhook when it is enabled
			if handler.Configuration.Webhook.Enabled {
				if err := lib.GetWebhook(handler.Configuration).Execute(hostname, ip, lastIP); err != nil {
					return err
				}
			}
		}
	}

	if len(updatedDomains) > 0 {
		successMessage := fmt.Sprintf("[ %s ] of %s", strings.Join(updatedDomains, ", "), domain.DomainName)
		handler.notificationManager.Send(successMessage, ip)
	}

	return nil
}
