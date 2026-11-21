package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// maxRouteIndex bounds the BROKER_ROUTE_<N>_* scan: indices beyond it are never
// examined. maxRoutes caps how many routes may be configured.
const (
	maxRouteIndex = 32
	maxRoutes     = 16
)

// loadRoutes reads BROKER_ROUTE_<N>_* (N = 1..maxRouteIndex), the per-model
// routing rules. An unset/empty BROKER_ROUTE_1_MODELS means routing is fully
// disabled (Routes stays nil), matching the UPSTREAM_UNIT_NAME
// empty-disables-it precedent (FR-9/AC-5).
func loadRoutes(cfg *Config) error {
	// Bounded scan to find the highest index with BROKER_ROUTE_<N>_MODELS
	// configured.
	highest := 0
	for i := 1; i <= maxRouteIndex; i++ {
		if getenv(fmt.Sprintf("BROKER_ROUTE_%d_MODELS", i), "") != "" {
			highest = i
		}
	}
	if highest == 0 {
		return nil
	}
	if highest > maxRoutes {
		return fmt.Errorf("too many configured routes: index %d exceeds max %d", highest, maxRoutes)
	}

	p := newRouteParser(cfg, highest)
	for i := 1; i <= highest; i++ {
		rb, err := p.parse(i, highest)
		if err != nil {
			return err
		}
		cfg.Routes = append(cfg.Routes, rb)
	}
	return nil
}

// routeParser holds the cross-instance uniqueness state: no two instances
// (default backend and/or any route) may share a resolved unit name or URL,
// and no model name may be routed by more than one rule.
type routeParser struct {
	seenModels    map[string]int
	seenUnitNames map[string]string
	seenURLs      map[string]string
}

func newRouteParser(cfg *Config, highest int) *routeParser {
	p := &routeParser{
		seenModels:    make(map[string]int, highest),
		seenUnitNames: make(map[string]string, highest+1),
		seenURLs:      make(map[string]string, highest+1),
	}
	defaultURL := cfg.UpstreamURL
	if cfg.UpstreamBackend == "ollama" {
		defaultURL = cfg.OllamaURL
	}
	if defaultURL != nil {
		p.seenURLs[defaultURL.String()] = "the default backend"
	}
	if cfg.UpstreamUnitName != "" {
		p.seenUnitNames[cfg.UpstreamUnitName] = "the default backend"
	}
	return p
}

// parse reads route i. highest is the highest configured index, named in the
// error for an index gap: any index below it without _MODELS set is a gap.
func (p *routeParser) parse(i, highest int) (RouteBackend, error) {
	prefix := fmt.Sprintf("BROKER_ROUTE_%d_", i)
	source := fmt.Sprintf("BROKER_ROUTE_%d", i)

	rawModels := getenv(prefix+"MODELS", "")
	if rawModels == "" {
		// An index gap fails loudly rather than silently limiting the table.
		return RouteBackend{}, fmt.Errorf("%sMODELS is unset but BROKER_ROUTE_%d_MODELS is configured (index gap not allowed)", prefix, highest)
	}
	models, err := p.parseModels(prefix, rawModels, i)
	if err != nil {
		return RouteBackend{}, err
	}

	routeBackend := getenv(prefix+"BACKEND", "openai")
	if routeBackend != "ollama" && routeBackend != "openai" {
		return RouteBackend{}, fmt.Errorf("%sBACKEND %q must be one of: ollama, openai", prefix, routeBackend)
	}
	routeURL, err := p.parseURL(prefix, source)
	if err != nil {
		return RouteBackend{}, err
	}
	routeAPIKey, err := parseRouteAPIKey(prefix)
	if err != nil {
		return RouteBackend{}, err
	}
	routeUnitName, err := p.parseUnitName(prefix, source)
	if err != nil {
		return RouteBackend{}, err
	}
	routeIdleTimeout, err := parseRouteIdle(prefix, routeUnitName)
	if err != nil {
		return RouteBackend{}, err
	}
	lane, err := parseRouteLane(prefix)
	if err != nil {
		return RouteBackend{}, err
	}

	return RouteBackend{
		Models:      models,
		Backend:     routeBackend,
		URL:         routeURL,
		APIKey:      routeAPIKey,
		UnitName:    routeUnitName,
		IdleTimeout: routeIdleTimeout,
		Lane:        lane,
		Displaces:   splitList(getenv(prefix+"DISPLACES", "")),
	}, nil
}

// parseRouteAPIKey reads a route's API key, which must not contain a CR or LF
// (HTTP header injection).
func parseRouteAPIKey(prefix string) (string, error) {
	key := getenv(prefix+"API_KEY", "")
	if strings.ContainsAny(key, "\r\n") {
		return "", fmt.Errorf("%sAPI_KEY must not contain control characters", prefix)
	}
	return key, nil
}

// parseRouteLane reads a route's optional lane scope.
func parseRouteLane(prefix string) (string, error) {
	lane := getenv(prefix+"LANE", "")
	if lane != "" && lane != "interactive" && lane != "batch" {
		return "", fmt.Errorf("%sLANE %q must be one of: \"\", interactive, batch", prefix, lane)
	}
	return lane, nil
}

// parseModels splits and checks a route's model list: no empty names, no
// repeats within the route, and no model claimed by an earlier route.
func (p *routeParser) parseModels(prefix, rawModels string, index int) ([]string, error) {
	parts := strings.Split(rawModels, ",")
	models := make([]string, 0, len(parts))
	localSeen := make(map[string]bool, len(parts))
	for _, part := range parts {
		m := strings.TrimSpace(part)
		if m == "" {
			return nil, fmt.Errorf("%sMODELS %q contains an empty model name", prefix, rawModels)
		}
		if localSeen[m] {
			return nil, fmt.Errorf("%sMODELS %q lists model %q more than once", prefix, rawModels, m)
		}
		localSeen[m] = true
		if existingSource, ok := p.seenModels[m]; ok {
			return nil, fmt.Errorf("model %q is routed by both BROKER_ROUTE_%d and BROKER_ROUTE_%d; a model may be routed to at most one backend", m, existingSource, index)
		}
		p.seenModels[m] = index
		models = append(models, m)
	}
	return models, nil
}

func (p *routeParser) parseURL(prefix, source string) (*url.URL, error) {
	rawURL := getenv(prefix+"URL", "")
	routeURL, err := parseBaseURL(prefix+"URL", rawURL)
	if err != nil {
		return nil, err
	}
	if existingSource, ok := p.seenURLs[routeURL.String()]; ok {
		return nil, fmt.Errorf("%sURL %q duplicates the URL already used by %s", prefix, rawURL, existingSource)
	}
	p.seenURLs[routeURL.String()] = source
	return routeURL, nil
}

func (p *routeParser) parseUnitName(prefix, source string) (string, error) {
	unitName := strings.TrimSpace(getenv(prefix+"UNIT_NAME", ""))
	if unitName == "" {
		return "", nil
	}
	if existingSource, ok := p.seenUnitNames[unitName]; ok {
		return "", fmt.Errorf("%sUNIT_NAME %q duplicates the unit name already used by %s", prefix, unitName, existingSource)
	}
	p.seenUnitNames[unitName] = source
	return unitName, nil
}

// parseRouteIdle reads a route's idle timeout, which must not be negative and
// needs a unit name to stop.
func parseRouteIdle(prefix, unitName string) (time.Duration, error) {
	idle, err := getdur(prefix+"IDLE_TIMEOUT", 0)
	if err != nil {
		return 0, err
	}
	if idle < 0 {
		return 0, fmt.Errorf("%sIDLE_TIMEOUT must not be negative, got %v", prefix, idle)
	}
	if idle != 0 && unitName == "" {
		return 0, fmt.Errorf("%sIDLE_TIMEOUT is set but %sUNIT_NAME is empty; idle-unload requires a unit name", prefix, prefix)
	}
	return idle, nil
}

// parseYieldUnits reads BROKER_YIELD_UNITS. A unit already stopped and
// started by the default backend or a route would get two controllers
// racing each other on every yield, so that is rejected, as is a repeat.
func parseYieldUnits(raw, upstreamUnit string, routes []RouteBackend) ([]string, error) {
	owners := map[string]string{}
	if upstreamUnit != "" {
		owners[upstreamUnit] = "UPSTREAM_UNIT_NAME"
	}
	for i, rb := range routes {
		if rb.UnitName != "" {
			owners[rb.UnitName] = fmt.Sprintf("BROKER_ROUTE_%d_UNIT_NAME", i+1)
		}
	}
	units := splitList(raw)
	for _, u := range units {
		if owner, ok := owners[u]; ok {
			return nil, fmt.Errorf("BROKER_YIELD_UNITS: %q is already used by %s", u, owner)
		}
		owners[u] = "BROKER_YIELD_UNITS"
	}
	return units, nil
}

// checkDisplaces validates every BROKER_ROUTE_<N>_DISPLACES (ADR-0020). A
// displaced unit must be a yield unit, since only those have a controller of
// their own to start and stop. A displacer needs an idle timeout, or it would
// keep the units off forever after its first request.
func checkDisplaces(routes []RouteBackend, yieldUnits []string) error {
	known := map[string]bool{}
	for _, u := range yieldUnits {
		known[u] = true
	}
	for i, rb := range routes {
		if len(rb.Displaces) == 0 {
			continue
		}
		key := fmt.Sprintf("BROKER_ROUTE_%d_DISPLACES", i+1)
		if rb.IdleTimeout == 0 {
			return fmt.Errorf("%s is set but BROKER_ROUTE_%d_IDLE_TIMEOUT is not; a displacing route must idle-unload to hand the GPU back", key, i+1)
		}
		seen := map[string]bool{}
		for _, u := range rb.Displaces {
			if !known[u] {
				return fmt.Errorf("%s: %q is not in BROKER_YIELD_UNITS", key, u)
			}
			if seen[u] {
				return fmt.Errorf("%s: %q is listed twice", key, u)
			}
			seen[u] = true
		}
	}
	return nil
}
