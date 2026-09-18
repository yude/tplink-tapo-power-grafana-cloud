'use strict';

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const test = require('node:test');

const propertyValues = new Map();
let uuidCounter = 0;
let fetchHandler = null;

global.Utilities = {
  DigestAlgorithm: { MD5: 'md5' },
  MacAlgorithm: { HMAC_SHA_1: 'sha1' },
  Charset: { UTF_8: 'utf8' },
  computeDigest(_algorithm, value) {
    return [...crypto.createHash('md5').update(value, 'utf8').digest()];
  },
  computeHmacSignature(_algorithm, value, key) {
    return [...crypto.createHmac('sha1', key).update(value, 'utf8').digest()];
  },
  base64Encode(value) {
    return Buffer.from(Array.isArray(value) ? value : String(value), Array.isArray(value) ? undefined : 'utf8').toString('base64');
  },
  getUuid() {
    uuidCounter += 1;
    return `00000000-0000-4000-8000-${String(uuidCounter).padStart(12, '0')}`;
  }
};

global.PropertiesService = {
  getScriptProperties() {
    return {
      getProperty: (key) => propertyValues.get(key) ?? null,
      setProperty(key, value) { propertyValues.set(key, String(value)); return this; },
      deleteProperty(key) { propertyValues.delete(key); return this; }
    };
  }
};

global.UrlFetchApp = {
  fetch(url, options) {
    if (!fetchHandler) throw new Error(`Unexpected fetch: ${url}`);
    return fetchHandler(url, options);
  }
};

global.LockService = {
  getScriptLock() {
    return { tryLock: () => true, waitLock() {}, releaseLock() {} };
  }
};

const app = require('../Code.js');

function response(status, body) {
  return {
    getResponseCode: () => status,
    getContentText: () => typeof body === 'string' ? body : JSON.stringify(body)
  };
}

function setRequiredProperties() {
  propertyValues.clear();
  propertyValues.set('TAPO_USERNAME', 'collector@example.invalid');
  propertyValues.set('TAPO_PASSWORD', 'not-a-real-password');
  propertyValues.set('GRAFANA_OTLP_ENDPOINT', 'https://otlp.example.invalid/otlp');
  propertyValues.set('GRAFANA_OTLP_INSTANCE_ID', '123456');
  propertyValues.set('GRAFANA_CLOUD_TOKEN', 'glc_test_only');
}

test.beforeEach(() => {
  uuidCounter = 0;
  fetchHandler = null;
  setRequiredProperties();
});

test('signing uses body MD5 and HMAC-SHA1 without leaking account credentials', () => {
  const payload = JSON.stringify({ method: 'getDeviceList' });
  const actual = app.signingHeaders_(payload, '/');
  const nonce = '00000000-0000-4000-8000-000000000001';
  const md5 = crypto.createHash('md5').update(payload).digest('base64');
  const input = [md5, '9999999999', nonce, '/'].join('\n');
  const signature = crypto.createHmac('sha1', app.TAPO_CLOUD.secretKey).update(input).digest('hex');
  assert.equal(actual.contentMd5, md5);
  assert.equal(actual.authorization,
    `Timestamp=9999999999, Nonce=${nonce}, AccessKey=${app.TAPO_CLOUD.accessKey}, Signature=${signature}`);
  assert.doesNotMatch(actual.authorization, /collector|password/i);
});

test('normalizes all documented realtime units', () => {
  assert.deepEqual(app.normalizeEnergyValue_(['get_emeter_data', 'voltage_mv'], 101234), {
    name: 'tapo_voltage_volts', unit: 'V', description: 'RMS voltage', value: 101.234
  });
  assert.equal(app.normalizeEnergyValue_(['get_energy_usage', 'current_power'], 1234).value, 1.234);
  assert.equal(app.normalizeEnergyValue_(['get_current_power', 'current_power'], 1.25).value, 1.25);
  assert.equal(app.normalizeEnergyValue_(['get_energy_usage', 'today_energy'], 42).unit, 'Wh');
  assert.equal(app.normalizeEnergyValue_(['get_energy_usage', 'today_runtime'], 3).value, 180);
});

test('preserves unknown numeric energy fields as labeled raw metrics', () => {
  const batch = app.newMetricBatch_(1700000000000);
  app.extractEnergyMetrics_(batch, {
    deviceId: 'device-1', alias: 'Desk', deviceModel: 'P110M'
  }, 'smart', {
    get_energy_usage: { current_power: 2500, new_firmware_counter: 7, err_code: 0 }
  }, batch.timestampMs);
  assert.equal(batch.metrics.tapo_power_watts.gauge.dataPoints[0].asDouble, 2.5);
  assert.equal(batch.metrics.tapo_energy_raw_value.gauge.dataPoints[0].asDouble, 7);
  assert.equal(batch.pointCount, 2);
});

test('host allowlist rejects redirects or API-supplied SSRF targets', () => {
  assert.doesNotThrow(() => app.assertAllowedTapoHost_('https://n-wap.i.tplinkcloud.com'));
  assert.throws(() => app.assertAllowedTapoHost_('https://evil.example/api'), /Rejected|path/);
  assert.throws(() => app.assertAllowedTapoHost_('https://user@x.tplinkcloud.com'), /credentials|path/);
  assert.throws(() => app.assertAllowedTapoHost_('http://n-wap.i.tplinkcloud.com'), /HTTPS/);
});

test('normalizes private-CA regional gateways to their publicly trusted aliases', () => {
  assert.equal(
    app.normalizeTapoHost_('https://n-aps1-wap-gw.tplinkcloud.com'),
    'https://aps1-wap-gw.tplinkcloud.com'
  );
  assert.equal(
    app.normalizeTapoHost_('https://use1-wap.tplinkcloud.com'),
    'https://use1-wap.tplinkcloud.com'
  );
  assert.throws(() => app.normalizeTapoHost_('https://evil.example'), /Rejected/);
});

test('restricts private-CA Thing API access to TP-Link app-server hosts', () => {
  assert.equal(
    app.normalizeThingHost_('https://aps1-app-server.iot.i.tplinkcloud.com/'),
    'https://aps1-app-server.iot.i.tplinkcloud.com'
  );
  assert.throws(() => app.normalizeThingHost_('https://aps1-wap.i.tplinkcloud.com'), /Rejected/);
  assert.throws(() => app.normalizeThingHost_('https://app-server.iot.i.tplinkcloud.com.evil.example'), /Rejected/);
});

test('new-terminal verification requests an email code and completes with the MFA process ID', () => {
  const calls = [];
  fetchHandler = (url, options) => {
    const body = JSON.parse(options.payload);
    calls.push({ url, body });
    if (url.includes('/api/v2/account/getAccountStatusAndUrl')) {
      return response(200, {
        error_code: 0,
        result: { appServerUrl: 'https://n-aps1-wap-gw.tplinkcloud.com' }
      });
    }
    if (url.includes('/api/v2/account/login')) {
      return response(200, {
        error_code: -20677,
        result: { MFAProcessId: 'process-1', supportedMFATypes: [1, 2] }
      });
    }
    if (url.includes('/api/v2/account/getEmailVC4TerminalMFA')) {
      return response(200, { error_code: 0, result: { errorCode: 0 } });
    }
    if (url.includes('/api/v2/account/checkMFACodeAndLogin')) {
      assert.deepEqual(body, {
        appType: 'TP-Link_Tapo_Android',
        cloudUserName: 'collector@example.invalid',
        code: '123456',
        MFAProcessId: 'process-1',
        MFAType: 2,
        terminalBindEnabled: true
      });
      return response(200, {
        error_code: 0,
        result: { token: 'token', refreshToken: 'refresh' }
      });
    }
    throw new Error(`Unexpected URL: ${url}`);
  };

  assert.deepEqual(app.initializeTapoSession(), {
    authenticated: false,
    terminalVerificationRequired: true,
    delivery: 'email',
    nextFunction: 'completeTapoMfa'
  });
  const pending = JSON.parse(propertyValues.get('_TAPO_PENDING_MFA_JSON'));
  assert.equal(pending.regionalUrl, 'https://aps1-wap-gw.tplinkcloud.com');
  assert.equal(pending.mfaProcessId, 'process-1');
  assert.equal(pending.mfaType, 2);
  assert.equal(JSON.stringify(pending).includes('not-a-real-password'), false);
  assert.ok(calls.some((call) => call.url.includes('/getEmailVC4TerminalMFA')));

  propertyValues.set('TAPO_MFA_CODE', '123456');
  assert.deepEqual(app.completeTapoMfa(), { authenticated: true });
  assert.equal(propertyValues.has('TAPO_MFA_CODE'), false);
  assert.equal(propertyValues.has('_TAPO_PENDING_MFA_JSON'), false);
  assert.equal(JSON.parse(propertyValues.get('_TAPO_SESSION_JSON')).token, 'token');
});

test('device selection is read-only, plug-scoped, and honors allowlist', () => {
  const devices = [
    { deviceId: 'a', deviceType: 'SMART.TAPOPLUG', deviceModel: 'P110', status: 1 },
    { deviceId: 'b', deviceType: 'SMART.IPCAMERA', deviceModel: 'C200', status: 1 },
    { deviceId: 'c', deviceType: 'SMART.TAPOPLUG', deviceModel: 'P115', status: 0 }
  ];
  assert.deepEqual(app.selectEnergyDevices_(devices, { deviceIds: [], includeOffline: false }).map((d) => d.deviceId), ['a', 'c']);
  assert.deepEqual(app.selectEnergyDevices_(devices, { deviceIds: ['c'], includeOffline: true }).map((d) => d.deviceId), ['c']);
  assert.equal(app.selectEnergyDevices_([
    { deviceId: 'd', device_type: 'other', device_model: 'P110M', deviceStatus: 'online' }
  ], { deviceIds: [], includeOffline: false }).length, 1);
  assert.deepEqual(app.selectBackfillDevices_([
    { deviceId: 'online', device_type: 'SMART.TAPOPLUG', device_model: 'P110M', deviceStatus: 'online' },
    { deviceId: 'offline', device_type: 'SMART.TAPOPLUG', device_model: 'P110M', deviceStatus: 'offline' }
  ], { deviceIds: [], includeOffline: false }).map((device) => device.deviceId), ['online', 'offline']);
  assert.equal(app.deviceOnlineState_({ deviceStatus: 'online' }), true);
  assert.equal(app.deviceOnlineState_({ online: false }), false);
  assert.equal(app.deviceOnlineState_({ status: 0 }), null);
  assert.equal(app.deviceOnlineState_({}), null);
});

test('OTLP JSON uses exact nanosecond strings and Basic authentication', () => {
  const batch = app.newMetricBatch_(1700000000123);
  app.extractEnergyMetrics_(batch, {
    deviceId: 'device-1', alias: 'Desk', deviceModel: 'P110M'
  }, 'smart', { get_energy_usage: { today_energy: 123 } }, batch.timestampMs);
  let captured;
  fetchHandler = (url, options) => {
    captured = { url, options, payload: JSON.parse(options.payload) };
    return response(200, {});
  };
  app.pushMetricBatch_({
    grafanaMetricsUrl: 'https://otlp.example.invalid/otlp/v1/metrics',
    grafanaInstanceId: '123456',
    grafanaToken: 'glc_test_only'
  }, batch);
  assert.equal(captured.url, 'https://otlp.example.invalid/otlp/v1/metrics');
  assert.equal(captured.options.headers.Authorization,
    `Basic ${Buffer.from('123456:glc_test_only').toString('base64')}`);
  const point = captured.payload.resourceMetrics[0].scopeMetrics[0].metrics[0].gauge.dataPoints[0];
  assert.equal(point.timeUnixNano, '1700000000123000000');
});

test('configuration uses the publicly trusted TP-Link endpoint and validates TLS by default', () => {
  const result = app.validateConfiguration();
  assert.equal(result.tapoInitialHost, 'https://wap.tplinkcloud.com');
  assert.equal(result.validatesTpLinkHttpsCertificates, true);
});

test('end-to-end collection performs only reads and sends the resulting OTLP batch', () => {
  const calls = [];
  fetchHandler = (url, options) => {
    const body = options.payload ? JSON.parse(options.payload) : null;
    calls.push({ url, options, body });
    if (url.includes('/api/v2/account/getAccountStatusAndUrl')) {
      return response(200, { error_code: 0, result: { appServerUrl: 'https://n-use1-wap.tplinkcloud.com' } });
    }
    if (url.includes('/api/v2/account/login')) {
      return response(200, { error_code: 0, result: { token: 'token', refreshToken: 'refresh' } });
    }
    if (url.includes('/api/v2/common/getDeviceListByPage')) {
      assert.equal(body.index, 0);
      assert.equal(body.limit, 100);
      assert.ok(body.deviceTypeList.includes('SMART.TAPOPLUG'));
      return response(200, {
        error_code: 0,
        result: {
          deviceList: [{
            deviceId: 'device-1',
            alias: 'Desk',
            deviceModel: 'P110M',
            deviceType: 'SMART.TAPOPLUG',
            appServerUrl: 'https://use1-wap.tplinkcloud.com',
            status: 1
          }]
        }
      });
    }
    if (url.startsWith('https://use1-wap.tplinkcloud.com/api/v2/common/passthrough')) {
      const requestData = JSON.parse(body.requestData);
      if (requestData.emeter) {
        return response(200, {
          error_code: 0,
          result: { responseData: JSON.stringify({ emeter: { get_realtime: { power_mw: 1200, err_code: 0 } } }) }
        });
      }
      return response(200, {
        error_code: 0,
        result: { responseData: JSON.stringify({ get_energy_usage: { current_power: 1250, today_energy: 10, err_code: 0 } }) }
      });
    }
    if (url === 'https://otlp.example.invalid/otlp/v1/metrics') {
      return response(200, {});
    }
    throw new Error(`Unexpected URL: ${url}`);
  };

  const result = app.runCollection();
  assert.equal(result.devicesSucceeded, 1);
  assert.equal(result.devicesFailed, 0);
  assert.ok(result.pointsSent >= 5);
  const tpLinkCalls = calls.filter((call) => call.url.includes('tplinkcloud.com'));
  assert.ok(tpLinkCalls.every((call) => call.options.validateHttpsCertificates === true));
  assert.ok(tpLinkCalls.every((call) => !/set_|turn_|toggle/.test(JSON.stringify(call.body))));
  const grafanaCall = calls.at(-1);
  assert.equal(grafanaCall.url, 'https://otlp.example.invalid/otlp/v1/metrics');
  assert.equal(grafanaCall.options.validateHttpsCertificates, true);
  assert.ok(grafanaCall.body.resourceMetrics[0].scopeMetrics[0].metrics.length > 0);
});

test('current P110M devices use the modern Thing API instead of offline legacy passthrough', () => {
  const calls = [];
  const thingHost = 'https://aps1-app-server.iot.i.tplinkcloud.com';
  fetchHandler = (url, options) => {
    const body = options.payload ? JSON.parse(options.payload) : null;
    calls.push({ url, options, body });
    if (url.includes('/api/v2/account/getAccountStatusAndUrl')) {
      return response(200, { error_code: 0, result: { appServerUrl: 'https://n-aps1-wap.tplinkcloud.com' } });
    }
    if (url.includes('/api/v2/account/login')) {
      return response(200, { error_code: 0, result: { token: 'token', refreshToken: 'refresh' } });
    }
    if (url.includes('/api/v2/common/getAppServiceUrlByCloudUserName')) {
      assert.deepEqual(body.serviceIds, app.TAPO_CLOUD.thingServiceIds);
      return response(200, {
        error_code: 0,
        result: {
          serviceList: [{
            serviceId: 'nbu.iot-app-server.app-v2',
            serviceUrl: thingHost
          }]
        }
      });
    }
    if (url.startsWith(`${thingHost}/v2/things?`)) {
      assert.equal(options.method, 'get');
      assert.equal(options.validateHttpsCertificates, false);
      assert.equal(options.headers.Authorization, 'ut|token');
      assert.match(options.headers['app-cid'], /^app:Tapo_Android:/);
      return response(200, {
        page: 0,
        pageSize: 100,
        total: 1,
        data: [{
          thingName: 'thing-1',
          nickname: 'RGVzaw==',
          model: 'P110M(JP)',
          category: 'plug.switch',
          appServerUrlV2: thingHost,
          status: 1
        }]
      });
    }
    if (url.startsWith(`${thingHost}/v1/things/thing-1/services-sync`)) {
      assert.equal(options.validateHttpsCertificates, false);
      assert.equal(body.serviceId, 'passthrough');
      const method = body.inputParams.requestData.method;
      if (method === 'get_energy_usage') {
        return response(200, {
          outputParams: {
            responseData: {
              result: {
                responses: [{
                  method,
                  error_code: 0,
                  result: { current_power: 1250, today_energy: 10 }
                }]
              }
            }
          }
        });
      }
      return response(200, {
        outputParams: { responseData: { error_code: -1002 } }
      });
    }
    if (url === 'https://otlp.example.invalid/otlp/v1/metrics') {
      return response(200, {});
    }
    throw new Error(`Unexpected URL: ${url}`);
  };

  const result = app.runCollection();
  assert.equal(result.devicesSucceeded, 1);
  assert.equal(result.devicesFailed, 0);
  assert.ok(result.pointsSent >= 4);
  assert.equal(calls.some((call) => call.url.includes('/api/v2/common/passthrough')), false);
  assert.equal(calls.some((call) => call.url.includes('/v1/things/thing-1/services-sync')), true);
});

test('source contains no device mutation command methods', () => {
  const source = require('node:fs').readFileSync(require('node:path').join(__dirname, '..', 'Code.js'), 'utf8');
  assert.doesNotMatch(source, /['"](?:set_relay_state|turn_on|turn_off|toggle|set_device_info)['"]/);
});

test('manifest requests only the external HTTP scope', () => {
  const manifest = JSON.parse(
    require('node:fs').readFileSync(require('node:path').join(__dirname, '..', 'appsscript.json'), 'utf8')
  );
  assert.deepEqual(manifest.oauthScopes, [
    'https://www.googleapis.com/auth/script.external_request'
  ]);
  const source = require('node:fs').readFileSync(require('node:path').join(__dirname, '..', 'Code.js'), 'utf8');
  assert.doesNotMatch(source, /\bScriptApp\b/);
});
