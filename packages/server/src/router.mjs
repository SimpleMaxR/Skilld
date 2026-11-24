export class HttpError extends Error {
  constructor(status, message, code = 'HTTP_ERROR') {
    super(message);
    this.status = status;
    this.code = code;
  }
}

export function jsonResponse(data, status = 200, extraHeaders = {}) {
  return new Response(JSON.stringify(data), {
    status,
    headers: {
      'content-type': 'application/json; charset=utf-8',
      'cache-control': 'no-store',
      ...corsHeaders(),
      ...extraHeaders,
    },
  });
}

export function textResponse(body, status = 200, contentType = 'text/plain; charset=utf-8') {
  return new Response(body, {
    status,
    headers: {
      'content-type': contentType,
      ...corsHeaders(),
    },
  });
}

export function corsHeaders() {
  return {
    'access-control-allow-origin': '*',
    'access-control-allow-methods': 'GET, POST, PUT, PATCH, DELETE, OPTIONS',
    'access-control-allow-headers': 'Content-Type, Authorization',
  };
}

export async function readJsonBody(request) {
  const text = await request.text();
  if (!text.trim()) return {};
  try {
    return JSON.parse(text);
  } catch {
    throw new HttpError(400, 'Invalid JSON request body', 'INVALID_JSON');
  }
}

export function createRouter() {
  const routes = [];

  function add(method, pattern, handler) {
    const parts = splitPath(pattern);
    routes.push({ method: method.toUpperCase(), parts, handler });
  }

  async function handle(request, ctx) {
    if (request.method === 'OPTIONS') {
      return new Response(null, { status: 204, headers: corsHeaders() });
    }

    const url = new URL(request.url);
    const reqParts = splitPath(url.pathname);
    const method = request.method.toUpperCase();

    for (const route of routes) {
      if (route.method !== method) continue;
      const params = matchParts(route.parts, reqParts);
      if (!params) continue;
      return route.handler({ request, url, params, ...ctx });
    }

    return null;
  }

  return {
    get: (p, h) => add('GET', p, h),
    post: (p, h) => add('POST', p, h),
    put: (p, h) => add('PUT', p, h),
    patch: (p, h) => add('PATCH', p, h),
    delete: (p, h) => add('DELETE', p, h),
    handle,
  };
}

function splitPath(pathname) {
  return pathname
    .split('/')
    .filter((seg) => seg.length > 0)
    .map((seg) => decodeURIComponent(seg));
}

function matchParts(patternParts, reqParts) {
  if (patternParts.length !== reqParts.length) return null;
  const params = {};
  for (let i = 0; i < patternParts.length; i++) {
    const pat = patternParts[i];
    const actual = reqParts[i];
    if (pat.startsWith(':')) {
      params[pat.slice(1)] = actual;
    } else if (pat !== actual) {
      return null;
    }
  }
  return params;
}
