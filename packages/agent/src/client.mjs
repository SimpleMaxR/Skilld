async function requestJson(baseUrl, token, path, { method = 'GET', body } = {}) {
  const cleanBase = String(baseUrl).replace(/\/+$/, '');
  const res = await fetch(`${cleanBase}${path}`, {
    method,
    headers: {
      'content-type': 'application/json',
      authorization: `Bearer ${token}`,
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });

  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    throw new Error(data.error || `HTTP ${res.status} on ${method} ${path}`);
  }
  return data;
}

export function createVaultClient({ serverUrl, token }) {
  return {
    verifyAuth() {
      return requestJson(serverUrl, token, '/api/auth/verify');
    },
    reportDevice(payload) {
      return requestJson(serverUrl, token, '/api/devices/report', {
        method: 'POST',
        body: payload,
      });
    },
    claimActions(deviceId) {
      return requestJson(
        serverUrl,
        token,
        `/api/devices/${encodeURIComponent(deviceId)}/actions/claim`,
        { method: 'POST', body: {} }
      );
    },
    completeAction(actionId, { status, errorMessage = null }) {
      return requestJson(
        serverUrl,
        token,
        `/api/actions/${encodeURIComponent(actionId)}/complete`,
        {
          method: 'POST',
          body: { status, errorMessage },
        }
      );
    },
  };
}
