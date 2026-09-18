/**
 * Read-only TP-Link Tapo energy collector for Google Apps Script.
 *
 * This client only issues account/device discovery and energy read methods.
 * It intentionally contains no relay, toggle, or device mutation methods.
 */

var TAPO_GRAFANA_VERSION = '0.1.5';

var TAPO_CLOUD = Object.freeze({
  initialHost: 'https://wap.tplinkcloud.com',
  appType: 'TP-Link_Tapo_Android',
  appVersion: '3.4.451',
  accessKey: '4d11b6b9d5ea4d19a829adbb9714b057',
  secretKey: '6ed7d97f3e73467f8a5bab90b577ba4c',
  signingTimestamp: '9999999999',
  accountStatusPath: '/api/v2/account/getAccountStatusAndUrl',
  loginPath: '/api/v2/account/login',
  refreshPath: '/api/v2/account/refreshToken',
  mfaEmailPath: '/api/v2/account/getEmailVC4TerminalMFA',
  mfaPath: '/api/v2/account/checkMFACodeAndLogin',
  passthroughPath: '/api/v2/common/passthrough',
  tokenExpired: -20651,
  refreshExpired: -20655,
  mfaRequired: -20677
});

var INTERNAL_PROPERTIES = Object.freeze({
  session: '_TAPO_SESSION_JSON',
  pendingMfa: '_TAPO_PENDING_MFA_JSON',
  terminalId: '_TAPO_TERMINAL_ID'
});

function TPLinkApiError_(message, code) {
  this.name = 'TPLinkApiError';
  this.message = message;
  this.code = typeof code === 'number' ? code : null;
  this.stack = new Error(message).stack;
}
TPLinkApiError_.prototype = Object.create(Error.prototype);
TPLinkApiError_.prototype.constructor = TPLinkApiError_;

function TPLinkTerminalVerificationRequired_(message) {
  this.name = 'TPLinkTerminalVerificationRequired';
  this.message = message;
  this.stack = new Error(message).stack;
}
TPLinkTerminalVerificationRequired_.prototype = Object.create(Error.prototype);
TPLinkTerminalVerificationRequired_.prototype.constructor = TPLinkTerminalVerificationRequired_;

/** Entry point for the installable time trigger. */
function runCollection() {
  var lock = LockService.getScriptLock();
  if (!lock.tryLock(5000)) {
    console.log('Another collection is already running; this invocation was skipped.');
    return { skipped: true };
  }

  try {
    var config = getConfig_();
    var session = getOrCreateSession_(config);
    var devicesResult = getDevicesWithRefresh_(config, session);
    session = devicesResult.session;
    var devices = selectEnergyDevices_(devicesResult.devices, config);
    var batch = newMetricBatch_(Date.now());
    var succeeded = 0;
    var failed = 0;

    devices.forEach(function (device) {
      addDeviceOnlineMetric_(batch, device);
      if (Number(device.status) !== 1) {
        addCollectionMetric_(batch, device, false);
        failed += 1;
        return;
      }

      try {
        var payloads = collectCurrentEnergy_(config, session, device);
        payloads.forEach(function (entry) {
          extractEnergyMetrics_(batch, device, entry.source, entry.payload, batch.timestampMs);
        });
        addCollectionMetric_(batch, device, true);
        succeeded += 1;
      } catch (error) {
        addCollectionMetric_(batch, device, false);
        failed += 1;
        console.error('Energy collection failed for ' + safeDeviceName_(device) + ': ' + safeError_(error));
      }
    });

    if (batch.pointCount === 0) {
      throw new Error('No metrics were produced. Check the device filter and device availability.');
    }
    pushMetricBatch_(config, batch);
    return {
      devicesSelected: devices.length,
      devicesSucceeded: succeeded,
      devicesFailed: failed,
      pointsSent: batch.pointCount
    };
  } finally {
    lock.releaseLock();
  }
}

/**
 * One-time/on-demand backfill for every history resolution exposed by current
 * Tapo energy firmware: this day (hourly), this quarter (daily), and this year
 * (monthly). Legacy emeter day/month history is collected as a fallback.
 */
function backfillEnergyHistory() {
  var lock = LockService.getScriptLock();
  lock.waitLock(30000);
  try {
    var config = getConfig_();
    var session = getOrCreateSession_(config);
    var devicesResult = getDevicesWithRefresh_(config, session);
    session = devicesResult.session;
    var devices = selectEnergyDevices_(devicesResult.devices, config).filter(function (device) {
      return Number(device.status) === 1;
    });
    var batch = newMetricBatch_(Date.now());
    var now = new Date();

    devices.forEach(function (device) {
      try {
        collectSmartHistory_(config, session, device, now, batch);
        collectLegacyHistory_(config, session, device, now, batch);
      } catch (error) {
        console.error('History collection failed for ' + safeDeviceName_(device) + ': ' + safeError_(error));
      }
    });

    if (batch.pointCount === 0) {
      throw new Error('No historical energy points were available from the selected devices.');
    }
    pushMetricBatch_(config, batch);
    return { devicesSelected: devices.length, pointsSent: batch.pointCount };
  } finally {
    lock.releaseLock();
  }
}

/** Validate local settings without contacting TP-Link or Grafana. */
function validateConfiguration() {
  var config = getConfig_();
  return {
    valid: true,
    tapoInitialHost: config.tapoInitialHost,
    grafanaMetricsUrl: config.grafanaMetricsUrl,
    deviceFilterCount: config.deviceIds.length,
    validatesTpLinkHttpsCertificates: !config.allowInsecureTls
  };
}

/**
 * Complete a TP-Link MFA challenge after setting TAPO_MFA_CODE in Script
 * Properties. The code property is deleted whether verification succeeds or
 * fails, so it is never retained accidentally.
 */
function completeTapoMfa() {
  var properties = PropertiesService.getScriptProperties();
  var config = getConfig_();
  var pendingText = properties.getProperty(INTERNAL_PROPERTIES.pendingMfa);
  var code = properties.getProperty('TAPO_MFA_CODE');
  if (!pendingText) {
    throw new Error('No pending MFA login. Run initializeTapoSession() first.');
  }
  if (!code) {
    throw new Error('Set TAPO_MFA_CODE in Script Properties before running this function.');
  }

  try {
    var pending = JSON.parse(pendingText);
    var body = {
      appType: TAPO_CLOUD.appType,
      cloudUserName: config.tapoUsername,
      code: code,
      MFAProcessId: pending.mfaProcessId,
      MFAType: pending.mfaType,
      terminalBindEnabled: true
    };
    var response = tapoPost_(config, pending.regionalUrl, TAPO_CLOUD.mfaPath, body, null, pending.terminalId);
    assertApiSuccess_(response, 'MFA verification');
    var session = sessionFromLoginResult_(pending.regionalUrl, pending.terminalId, response.result || {});
    saveSession_(session);
    properties.deleteProperty(INTERNAL_PROPERTIES.pendingMfa);
    return { authenticated: true };
  } finally {
    properties.deleteProperty('TAPO_MFA_CODE');
  }
}

/** Start a new login explicitly; useful during first-time setup. */
function initializeTapoSession() {
  clearSession_();
  try {
    getOrCreateSession_(getConfig_());
    return { authenticated: true, terminalVerificationRequired: false };
  } catch (error) {
    if (!(error instanceof TPLinkTerminalVerificationRequired_)) throw error;
    console.log(error.message);
    return {
      authenticated: false,
      terminalVerificationRequired: true,
      delivery: 'email',
      nextFunction: 'completeTapoMfa'
    };
  }
}

function getConfig_() {
  var p = PropertiesService.getScriptProperties();
  var endpoint = requiredProperty_(p, 'GRAFANA_OTLP_ENDPOINT').replace(/\/+$/, '');
  var metricsUrl;
  if (/\/v1\/metrics$/.test(endpoint)) {
    metricsUrl = endpoint;
  } else if (/\/otlp$/.test(endpoint)) {
    metricsUrl = endpoint + '/v1/metrics';
  } else {
    throw new Error('GRAFANA_OTLP_ENDPOINT must end with /otlp or /otlp/v1/metrics.');
  }
  assertHttpsUrl_(metricsUrl, 'GRAFANA_OTLP_ENDPOINT');

  var allowInsecureTls = p.getProperty('TAPO_ALLOW_INSECURE_TLS') === 'true';

  return {
    tapoUsername: requiredProperty_(p, 'TAPO_USERNAME'),
    tapoPassword: requiredProperty_(p, 'TAPO_PASSWORD'),
    tapoInitialHost: normalizeTapoHost_(p.getProperty('TAPO_CLOUD_HOST') || TAPO_CLOUD.initialHost),
    tapoAppVersion: p.getProperty('TAPO_APP_VERSION') || TAPO_CLOUD.appVersion,
    allowInsecureTls: allowInsecureTls,
    grafanaMetricsUrl: metricsUrl,
    grafanaInstanceId: requiredProperty_(p, 'GRAFANA_OTLP_INSTANCE_ID'),
    grafanaToken: requiredProperty_(p, 'GRAFANA_CLOUD_TOKEN'),
    deviceIds: splitCsv_(p.getProperty('TAPO_DEVICE_IDS') || ''),
    includeOffline: p.getProperty('TAPO_INCLUDE_OFFLINE') === 'true'
  };
}

function requiredProperty_(properties, name) {
  var value = properties.getProperty(name);
  if (!value) throw new Error('Missing Script Property: ' + name);
  return value;
}

function splitCsv_(value) {
  return value.split(',').map(function (item) { return item.trim(); }).filter(Boolean);
}

function getOrCreateSession_(config) {
  var properties = PropertiesService.getScriptProperties();
  var saved = properties.getProperty(INTERNAL_PROPERTIES.session);
  if (saved) {
    try {
      var parsed = JSON.parse(saved);
      if (parsed.token && parsed.regionalUrl && parsed.terminalId) return parsed;
    } catch (ignored) {
      properties.deleteProperty(INTERNAL_PROPERTIES.session);
    }
  }
  return login_(config);
}

function login_(config) {
  var properties = PropertiesService.getScriptProperties();
  var terminalId = properties.getProperty(INTERNAL_PROPERTIES.terminalId) || Utilities.getUuid();
  properties.setProperty(INTERNAL_PROPERTIES.terminalId, terminalId);

  assertAllowedTapoHost_(config.tapoInitialHost);
  var statusResponse = tapoPost_(config, config.tapoInitialHost, TAPO_CLOUD.accountStatusPath, {
    appType: TAPO_CLOUD.appType,
    cloudUserName: config.tapoUsername
  }, null, terminalId);
  assertApiSuccess_(statusResponse, 'regional endpoint discovery');
  var regionalUrl = normalizeTapoHost_((statusResponse.result || {}).appServerUrl || config.tapoInitialHost);

  var loginResponse = tapoPost_(config, regionalUrl, TAPO_CLOUD.loginPath, {
    appType: TAPO_CLOUD.appType,
    appVersion: config.tapoAppVersion,
    cloudPassword: config.tapoPassword,
    cloudUserName: config.tapoUsername,
    platform: 'Android',
    refreshTokenNeeded: true,
    supportBindAccount: false,
    terminalUUID: terminalId,
    terminalName: 'Google Apps Script',
    terminalMeta: 'Google Apps Script'
  }, null, terminalId);

  var code = apiErrorCode_(loginResponse);
  if (code === TAPO_CLOUD.mfaRequired) {
    var loginResult = loginResponse.result || {};
    var processId = loginResult.MFAProcessId || loginResult.mfaProcessId;
    if (!processId) {
      throw new Error('TP-Link requested terminal verification but returned no MFA process ID.');
    }
    var supportedTypes = normalizeMfaTypes_(loginResult.supportedMFATypes || loginResult.supportedMfaTypes || []);
    if (supportedTypes.length && supportedTypes.indexOf(2) === -1) {
      throw new Error(
        'TP-Link requested terminal verification, but email verification is not available. Supported types: ' +
        supportedTypes.join(', ')
      );
    }
    var mfaType = 2;
    var sendResponse = tapoPost_(config, regionalUrl, TAPO_CLOUD.mfaEmailPath, {
      appType: TAPO_CLOUD.appType,
      cloudPassword: config.tapoPassword,
      cloudUserName: config.tapoUsername,
      terminalUUID: terminalId
    }, null, terminalId);
    assertApiSuccess_(sendResponse, 'email terminal verification code request');
    properties.setProperty(INTERNAL_PROPERTIES.pendingMfa, JSON.stringify({
      regionalUrl: regionalUrl,
      terminalId: terminalId,
      mfaProcessId: processId,
      mfaType: mfaType
    }));
    throw new TPLinkTerminalVerificationRequired_(
      'TP-Link requires verification for this new terminal even if account 2-step verification is disabled. ' +
      'A verification code was requested by email. Set TAPO_MFA_CODE, then run completeTapoMfa().'
    );
  }
  assertApiSuccess_(loginResponse, 'TP-Link login');
  var session = sessionFromLoginResult_(regionalUrl, terminalId, loginResponse.result || {});
  saveSession_(session);
  return session;
}

function sessionFromLoginResult_(regionalUrl, terminalId, result) {
  if (!result.token) throw new Error('TP-Link login succeeded without returning a token.');
  return {
    regionalUrl: regionalUrl,
    terminalId: terminalId,
    token: result.token,
    refreshToken: result.refreshToken || null,
    savedAt: Date.now()
  };
}

function saveSession_(session) {
  PropertiesService.getScriptProperties().setProperty(INTERNAL_PROPERTIES.session, JSON.stringify(session));
}

function clearSession_() {
  var p = PropertiesService.getScriptProperties();
  p.deleteProperty(INTERNAL_PROPERTIES.session);
  p.deleteProperty(INTERNAL_PROPERTIES.pendingMfa);
}

function refreshSession_(config, session) {
  if (!session.refreshToken) return login_(config);
  var response = tapoPost_(config, session.regionalUrl, TAPO_CLOUD.refreshPath, {
    appType: TAPO_CLOUD.appType,
    refreshToken: session.refreshToken,
    terminalUUID: session.terminalId
  }, null, session.terminalId);
  if (apiErrorCode_(response) === TAPO_CLOUD.refreshExpired) return login_(config);
  assertApiSuccess_(response, 'token refresh');
  var refreshed = sessionFromLoginResult_(session.regionalUrl, session.terminalId, response.result || {});
  saveSession_(refreshed);
  return refreshed;
}

function getDevicesWithRefresh_(config, session) {
  try {
    return { devices: getDevices_(config, session), session: session };
  } catch (error) {
    if (!(error instanceof TPLinkApiError_) || error.code !== TAPO_CLOUD.tokenExpired) throw error;
    var refreshed = refreshSession_(config, session);
    return { devices: getDevices_(config, refreshed), session: refreshed };
  }
}

function getDevices_(config, session) {
  var response = tapoPost_(config, session.regionalUrl, '/', { method: 'getDeviceList' }, session.token, session.terminalId);
  assertApiSuccess_(response, 'device listing');
  return (response.result || {}).deviceList || [];
}

function selectEnergyDevices_(devices, config) {
  return devices.filter(function (device) {
    if (config.deviceIds.length && config.deviceIds.indexOf(String(device.deviceId)) === -1) return false;
    if (!config.includeOffline && Number(device.status) !== 1) return false;
    var kind = String(device.deviceType || '').toUpperCase();
    var model = String(device.deviceModel || '').toUpperCase();
    return /PLUG|SWITCH|STRIP/.test(kind) || /^(P|KP|EP|HS)/.test(model);
  });
}

function collectCurrentEnergy_(config, session, device) {
  var requests = [
    {
      source: 'smart',
      body: {
        get_energy_usage: null,
        get_current_power: null,
        get_emeter_data: null,
        get_emeter_vgain_igain: null,
        get_device_usage: null
      }
    },
    {
      source: 'legacy_emeter',
      body: { emeter: { get_realtime: null } }
    }
  ];
  var results = [];
  var errors = [];
  requests.forEach(function (request) {
    try {
      results.push({
        source: request.source,
        payload: tapoPassthrough_(config, session, device, request.body)
      });
    } catch (error) {
      errors.push(request.source + ': ' + safeError_(error));
    }
  });
  if (!results.length) throw new Error(errors.join('; '));
  return results;
}

function tapoPassthrough_(config, session, device, requestData) {
  var host = normalizeTapoHost_(device.appServerUrl || session.regionalUrl);
  var response = tapoPost_(config, host, TAPO_CLOUD.passthroughPath, {
    deviceId: device.deviceId,
    requestData: JSON.stringify(requestData)
  }, session.token, session.terminalId);
  assertApiSuccess_(response, 'device energy read');
  var responseData = (response.result || {}).responseData;
  if (typeof responseData === 'string') responseData = JSON.parse(responseData);
  if (!responseData || typeof responseData !== 'object') {
    throw new Error('Device returned no energy response data.');
  }
  return responseData;
}

function tapoPost_(config, host, path, body, token, terminalId) {
  host = normalizeTapoHost_(host);
  var payload = JSON.stringify(body);
  var signing = signingHeaders_(payload, path);
  var params = {
    appName: TAPO_CLOUD.appType,
    appVer: config.tapoAppVersion,
    netType: 'wifi',
    termID: terminalId,
    ospf: 'Android 14',
    brand: 'TPLINK',
    locale: 'en_US',
    model: 'GoogleAppsScript',
    termName: 'GoogleAppsScript',
    termMeta: 'GoogleAppsScript'
  };
  if (token) params.token = token;
  var url = host + (path === '/' ? '/' : path) + '?' + encodeQuery_(params);
  var response = UrlFetchApp.fetch(url, {
    method: 'post',
    contentType: 'application/json;charset=UTF-8',
    headers: {
      'Content-MD5': signing.contentMd5,
      'X-Authorization': signing.authorization
    },
    payload: payload,
    validateHttpsCertificates: !config.allowInsecureTls,
    followRedirects: false,
    muteHttpExceptions: true,
    timeoutSeconds: 60
  });
  var status = response.getResponseCode();
  if (status < 200 || status >= 300) {
    throw new Error('TP-Link API returned HTTP ' + status + '.');
  }
  try {
    return JSON.parse(response.getContentText());
  } catch (error) {
    throw new Error('TP-Link API returned invalid JSON.');
  }
}

function signingHeaders_(payload, path) {
  var md5Bytes = Utilities.computeDigest(Utilities.DigestAlgorithm.MD5, payload, Utilities.Charset.UTF_8);
  var contentMd5 = Utilities.base64Encode(md5Bytes);
  var nonce = Utilities.getUuid();
  var signatureInput = [contentMd5, TAPO_CLOUD.signingTimestamp, nonce, path].join('\n');
  var signatureBytes = Utilities.computeHmacSignature(
    Utilities.MacAlgorithm.HMAC_SHA_1,
    signatureInput,
    TAPO_CLOUD.secretKey,
    Utilities.Charset.UTF_8
  );
  var signature = bytesToHex_(signatureBytes);
  return {
    contentMd5: contentMd5,
    authorization: 'Timestamp=' + TAPO_CLOUD.signingTimestamp +
      ', Nonce=' + nonce +
      ', AccessKey=' + TAPO_CLOUD.accessKey +
      ', Signature=' + signature
  };
}

function bytesToHex_(bytes) {
  return bytes.map(function (value) {
    return ('0' + (value & 255).toString(16)).slice(-2);
  }).join('');
}

function encodeQuery_(params) {
  return Object.keys(params).sort().map(function (key) {
    return encodeURIComponent(key) + '=' + encodeURIComponent(String(params[key]));
  }).join('&');
}

function apiErrorCode_(response) {
  var outer = Number(response && response.error_code || 0);
  if (outer) return outer;
  var inner = response && response.result && response.result.errorCode;
  return inner === undefined || inner === null ? 0 : Number(inner);
}

function normalizeMfaTypes_(types) {
  if (!Array.isArray(types)) return [];
  return types.map(function (value) {
    var normalized = String(value).toLowerCase();
    if (normalized === 'email') return 2;
    if (normalized === 'push') return 1;
    var numeric = Number(value);
    return isFinite(numeric) ? numeric : null;
  }).filter(function (value) { return value !== null; });
}

function assertApiSuccess_(response, operation) {
  var code = apiErrorCode_(response);
  if (code !== 0) {
    var message = (response && response.msg) ||
      (response && response.result && response.result.errorMsg) ||
      operation + ' failed';
    throw new TPLinkApiError_(operation + ' failed (' + code + '): ' + sanitizeText_(message), code);
  }
}

function assertAllowedTapoHost_(url) {
  assertHttpsUrl_(url, 'TP-Link API URL');
  if (!/^https:\/\/[a-z0-9.-]+(?::\d+)?$/i.test(url)) {
    throw new Error('TP-Link API URL must not contain a path, query, or credentials.');
  }
  var host = url.replace(/^https:\/\//i, '').replace(/:\d+$/, '').toLowerCase();
  var allowed = host === 'tplinkcloud.com' || host.endsWith('.tplinkcloud.com') ||
    host === 'tplinknbu.com' || host.endsWith('.tplinknbu.com');
  if (!allowed) throw new Error('Rejected unexpected TP-Link API host: ' + host);
}

/**
 * TP-Link's discovery API can return an `n-` gateway whose certificate chains
 * to a private TP-Link root. The otherwise identical hostname without `n-`
 * presents a publicly trusted certificate. Only rewrite that exact regional
 * tplinkcloud.com pattern; all other hosts still pass through the allowlist.
 */
function normalizeTapoHost_(url) {
  var normalized = String(url).replace(/\/+$/, '');
  assertAllowedTapoHost_(normalized);
  return normalized.replace(
    /^https:\/\/n-([a-z0-9-]+\.tplinkcloud\.com)(:\d+)?$/i,
    'https://$1$2'
  );
}

function assertHttpsUrl_(url, name) {
  if (!/^https:\/\//i.test(url)) throw new Error(name + ' must use HTTPS.');
  if (/^https:\/\/[^/]*@/i.test(url)) throw new Error(name + ' must not contain credentials.');
}

function newMetricBatch_(timestampMs) {
  return { timestampMs: timestampMs, metrics: {}, pointCount: 0 };
}

function addMetricPoint_(batch, name, unit, description, value, attributes, timestampMs) {
  if (typeof value !== 'number' || !isFinite(value)) return;
  if (!batch.metrics[name]) {
    batch.metrics[name] = {
      name: name,
      unit: unit || '1',
      description: description || '',
      gauge: { dataPoints: [] }
    };
  }
  batch.metrics[name].gauge.dataPoints.push({
    attributes: attributesToOtlp_(attributes),
    timeUnixNano: String(Math.floor(timestampMs)) + '000000',
    asDouble: value
  });
  batch.pointCount += 1;
}

function deviceAttributes_(device, source) {
  var attributes = {
    'tapo.device.id': String(device.deviceId || 'unknown'),
    'tapo.device.name': safeDeviceName_(device),
    'tapo.device.model': String(device.deviceModel || 'unknown')
  };
  if (source) attributes['tapo.data.source'] = source;
  return attributes;
}

function addDeviceOnlineMetric_(batch, device) {
  addMetricPoint_(batch, 'tapo_device_online', '1', 'Whether TP-Link Cloud reports the device online',
    Number(device.status) === 1 ? 1 : 0, deviceAttributes_(device), batch.timestampMs);
}

function addCollectionMetric_(batch, device, success) {
  addMetricPoint_(batch, 'tapo_energy_collection_success', '1', 'Whether all usable energy reads completed',
    success ? 1 : 0, deviceAttributes_(device), batch.timestampMs);
}

function extractEnergyMetrics_(batch, device, source, payload, timestampMs) {
  walkNumericLeaves_(payload, [], function (path, value) {
    var leaf = path[path.length - 1] || '';
    if (/^(err_code|error_code|start_timestamp|end_timestamp|local_time|interval|year|month|day)$/i.test(leaf)) return;
    var normalized = normalizeEnergyValue_(path, value);
    var attributes = deviceAttributes_(device, source);
    if (normalized) {
      addMetricPoint_(batch, normalized.name, normalized.unit, normalized.description,
        normalized.value, attributes, timestampMs);
    } else {
      attributes['tapo.energy.field'] = path.join('.');
      addMetricPoint_(batch, 'tapo_energy_raw_value', '1', 'Unnormalized numeric value returned by a Tapo energy API',
        value, attributes, timestampMs);
    }
  });
}

function walkNumericLeaves_(value, path, visitor) {
  if (typeof value === 'number') {
    visitor(path, value);
    return;
  }
  if (!value || typeof value !== 'object') return;
  if (Array.isArray(value)) {
    value.forEach(function (item, index) { walkNumericLeaves_(item, path.concat(String(index)), visitor); });
    return;
  }
  Object.keys(value).forEach(function (key) {
    walkNumericLeaves_(value[key], path.concat(key), visitor);
  });
}

function normalizeEnergyValue_(path, value) {
  var leaf = String(path[path.length - 1] || '').toLowerCase();
  var joined = path.join('.').toLowerCase();
  if (leaf === 'voltage_mv') return normalized_('tapo_voltage_volts', 'V', 'RMS voltage', value / 1000);
  if (leaf === 'voltage') return normalized_('tapo_voltage_volts', 'V', 'RMS voltage', value);
  if (leaf === 'current_ma') return normalized_('tapo_current_amperes', 'A', 'RMS current', value / 1000);
  if (leaf === 'current') return normalized_('tapo_current_amperes', 'A', 'RMS current', value);
  if (leaf === 'power_mw') return normalized_('tapo_power_watts', 'W', 'Instantaneous active power', value / 1000);
  if (leaf === 'current_power' && joined.indexOf('get_energy_usage') !== -1) {
    return normalized_('tapo_power_watts', 'W', 'Instantaneous active power', value / 1000);
  }
  if (leaf === 'current_power' || leaf === 'power') {
    return normalized_('tapo_power_watts', 'W', 'Instantaneous active power', value);
  }
  if (leaf === 'today_energy') return normalized_('tapo_energy_today_watt_hours', 'Wh', 'Energy used today', value);
  if (leaf === 'month_energy') return normalized_('tapo_energy_month_watt_hours', 'Wh', 'Energy used this month', value);
  if (leaf === 'total_wh') return normalized_('tapo_energy_total_watt_hours', 'Wh', 'Total energy reported by the device', value);
  if (leaf === 'total') return normalized_('tapo_energy_total_watt_hours', 'Wh', 'Total energy reported by a legacy meter', value * 1000);
  if (leaf === 'today_runtime') return normalized_('tapo_runtime_today_seconds', 's', 'Powered runtime today', value * 60);
  if (leaf === 'month_runtime') return normalized_('tapo_runtime_month_seconds', 's', 'Powered runtime this month', value * 60);
  if (leaf === 'energy_wh') return normalized_('tapo_energy_watt_hours', 'Wh', 'Energy value returned by the device', value);
  return null;
}

function normalized_(name, unit, description, value) {
  return { name: name, unit: unit, description: description, value: value };
}

function attributesToOtlp_(attributes) {
  return Object.keys(attributes).sort().map(function (key) {
    return { key: key, value: { stringValue: String(attributes[key]) } };
  });
}

function pushMetricBatch_(config, batch) {
  var metrics = Object.keys(batch.metrics).sort().map(function (name) { return batch.metrics[name]; });
  var payload = {
    resourceMetrics: [{
      resource: {
        attributes: attributesToOtlp_({
          'service.name': 'tapo-grafana-gas',
          'service.version': TAPO_GRAFANA_VERSION,
          'service.instance.id': 'google-apps-script'
        })
      },
      scopeMetrics: [{
        scope: { name: 'tapo-grafana-gas', version: TAPO_GRAFANA_VERSION },
        metrics: metrics
      }]
    }]
  };
  var auth = Utilities.base64Encode(config.grafanaInstanceId + ':' + config.grafanaToken);
  var response = UrlFetchApp.fetch(config.grafanaMetricsUrl, {
    method: 'post',
    contentType: 'application/json',
    headers: { Authorization: 'Basic ' + auth },
    payload: JSON.stringify(payload),
    muteHttpExceptions: true,
    followRedirects: false,
    validateHttpsCertificates: true,
    timeoutSeconds: 60
  });
  var status = response.getResponseCode();
  if (status < 200 || status >= 300) {
    throw new Error('Grafana Cloud OTLP endpoint returned HTTP ' + status + '.');
  }
}

function collectSmartHistory_(config, session, device, now, batch) {
  var windows = historyWindows_(now);
  windows.forEach(function (window) {
    collectSmartHistoryWindow_(config, session, device, window, batch);
  });
}

function collectSmartHistoryWindow_(config, session, device, window, batch) {
  var requestedEnd = Math.floor(window.end.getTime() / 1000);
  var nextStart = Math.floor(window.start.getTime() / 1000);
  var seen = {};

  for (var page = 0; page < 12 && nextStart < requestedEnd; page += 1) {
    var request = {
      get_energy_data: {
        start_timestamp: nextStart,
        end_timestamp: requestedEnd,
        interval: window.intervalMinutes
      }
    };
    var payload;
    try {
      payload = tapoPassthrough_(config, session, device, request);
    } catch (ignored) {
      return;
    }
    var result = payload.get_energy_data || payload;
    if (!result || !Array.isArray(result.data)) return;
    var pageStart = Number(result.start_timestamp || nextStart);
    result.data.forEach(function (value, index) {
      if (typeof value !== 'number') return;
      var timestampMs = historyBucketTimestamp_(pageStart, window.resolution, window.intervalMinutes, index);
      var key = window.resolution + ':' + timestampMs;
      if (seen[key]) return;
      seen[key] = true;
      var attributes = deviceAttributes_(device, 'smart_history');
      attributes['tapo.energy.resolution'] = window.resolution;
      addMetricPoint_(batch, 'tapo_energy_bucket_watt_hours', 'Wh', 'Energy consumed in a historical time bucket',
        value, attributes, timestampMs);
    });

    var returnedEnd = Number(result.end_timestamp || requestedEnd);
    if (!isFinite(returnedEnd) || returnedEnd <= nextStart || returnedEnd >= requestedEnd) return;
    nextStart = returnedEnd;
  }
}

function historyBucketTimestamp_(pageStartSeconds, resolution, intervalMinutes, index) {
  var start = new Date(pageStartSeconds * 1000);
  if (resolution === 'monthly') {
    return new Date(start.getFullYear(), start.getMonth() + index, 1).getTime();
  }
  return (pageStartSeconds + index * intervalMinutes * 60) * 1000;
}

function collectLegacyHistory_(config, session, device, now, batch) {
  var dayPayload = {};
  var monthPayload = {};
  try {
    dayPayload = tapoPassthrough_(config, session, device, {
      emeter: { get_daystat: { year: now.getFullYear(), month: now.getMonth() + 1 } }
    });
  } catch (ignoredDay) {}
  try {
    monthPayload = tapoPassthrough_(config, session, device, {
      emeter: { get_monthstat: { year: now.getFullYear() } }
    });
  } catch (ignoredMonth) {}
  var days = ((((dayPayload || {}).emeter || {}).get_daystat || {}).day_list) || [];
  days.forEach(function (item) {
    var value = numberFrom_(item, ['energy_wh', 'energy']);
    if (value === null) return;
    if (item.energy_wh === undefined) value *= 1000;
    var attributes = deviceAttributes_(device, 'legacy_history');
    attributes['tapo.energy.resolution'] = 'daily';
    addMetricPoint_(batch, 'tapo_energy_bucket_watt_hours', 'Wh', 'Energy consumed in a historical time bucket',
      value, attributes, new Date(item.year, item.month - 1, item.day).getTime());
  });
  var months = ((((monthPayload || {}).emeter || {}).get_monthstat || {}).month_list) || [];
  months.forEach(function (item) {
    var value = numberFrom_(item, ['energy_wh', 'energy']);
    if (value === null) return;
    if (item.energy_wh === undefined) value *= 1000;
    var attributes = deviceAttributes_(device, 'legacy_history');
    attributes['tapo.energy.resolution'] = 'monthly';
    addMetricPoint_(batch, 'tapo_energy_bucket_watt_hours', 'Wh', 'Energy consumed in a historical time bucket',
      value, attributes, new Date(item.year, item.month - 1, 1).getTime());
  });
}

function historyWindows_(now) {
  var year = now.getFullYear();
  var month = now.getMonth();
  var quarterStart = Math.floor(month / 3) * 3;
  return [
    {
      resolution: 'hourly', intervalMinutes: 60,
      start: new Date(year, month, now.getDate()),
      end: new Date(year, month, now.getDate() + 1)
    },
    {
      resolution: 'daily', intervalMinutes: 1440,
      start: new Date(year, quarterStart, 1),
      end: new Date(year, quarterStart + 3, 1)
    },
    {
      resolution: 'monthly', intervalMinutes: 43200,
      start: new Date(year, 0, 1),
      end: new Date(year + 1, 0, 1)
    }
  ];
}

function numberFrom_(object, keys) {
  for (var i = 0; i < keys.length; i += 1) {
    if (typeof object[keys[i]] === 'number') return object[keys[i]];
  }
  return null;
}

function safeDeviceName_(device) {
  return sanitizeText_(device.alias || device.deviceName || device.deviceModel || 'unknown');
}

function sanitizeText_(value) {
  return String(value).replace(/[\r\n\t]/g, ' ').slice(0, 200);
}

function safeError_(error) {
  return sanitizeText_(error && error.message ? error.message : error);
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    TAPO_CLOUD: TAPO_CLOUD,
    signingHeaders_: signingHeaders_,
    normalizeEnergyValue_: normalizeEnergyValue_,
    extractEnergyMetrics_: extractEnergyMetrics_,
    historyWindows_: historyWindows_,
    newMetricBatch_: newMetricBatch_,
    pushMetricBatch_: pushMetricBatch_,
    apiErrorCode_: apiErrorCode_,
    normalizeMfaTypes_: normalizeMfaTypes_,
    assertAllowedTapoHost_: assertAllowedTapoHost_,
    normalizeTapoHost_: normalizeTapoHost_,
    selectEnergyDevices_: selectEnergyDevices_,
    runCollection: runCollection,
    initializeTapoSession: initializeTapoSession,
    completeTapoMfa: completeTapoMfa,
    validateConfiguration: validateConfiguration
  };
}
