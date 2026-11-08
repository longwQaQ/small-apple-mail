let csrfToken = '';

export class APIError extends Error {
  constructor(message, {code = 'request_failed', retryable = false, requestID = '', status = 0, details = {}} = {}) {
    super(message); this.name = 'APIError'; this.code = code; this.retryable = retryable; this.requestID = requestID; this.status = status; this.details = details;
  }
}

export function setCSRF(value) { csrfToken = value || ''; }

export async function request(path, options = {}) {
  const method = String(options.method || 'GET').toUpperCase();
  const headers = new Headers(options.headers || {});
  if (options.body != null && !(options.body instanceof FormData)) headers.set('content-type', 'application/json');
  if (csrfToken && !['GET', 'HEAD', 'OPTIONS'].includes(method)) headers.set('X-CSRF-Token', csrfToken);
  const response = await fetch(path, {...options, method, headers, credentials: 'same-origin'});
  const requestID = response.headers.get('X-Request-ID') || '';
  if (response.status === 204) return null;
  let payload;
  try { payload = await response.json(); } catch { throw new APIError(`服务器返回了无法解析的响应（HTTP ${response.status}）`, {status: response.status, requestID}); }
  if (response.status === 401 && !path.endsWith('/session')) { location.href = '/login'; throw new APIError('登录状态已失效', {status: 401, code: 'auth_required', requestID}); }
  if (!response.ok || payload.error) {
    const error = payload.error || {};
    throw new APIError(error.message || `请求失败（HTTP ${response.status}）`, {code: error.code, retryable: !!error.retryable, requestID: error.request_id || requestID, status: response.status, details: error.details || {}});
  }
  return payload.data;
}

export const api = {
  session: () => request('/api/admin/session'),
  logout: () => request('/api/admin/session', {method: 'DELETE'}),
  dashboard: () => request('/api/admin/dashboard'),
  accounts: async () => (await request('/api/admin/accounts')) || [],
  createAccount: body => request('/api/admin/accounts', {method: 'POST', body: JSON.stringify(body)}),
  previewAccountDelete: accountID => request(`/api/admin/accounts/${encodeURIComponent(accountID)}/delete-preview`),
  deleteAccount: (accountID, confirmation_token) => request(`/api/admin/accounts/${encodeURIComponent(accountID)}`, {method: 'DELETE', body: JSON.stringify({confirmation_token})}),
  saveConnection: (accountID, kind, body) => request(`/api/admin/accounts/${encodeURIComponent(accountID)}/connections/${kind}`, {method: 'PUT', body: JSON.stringify(body)}),
  verifyConnection: (accountID, kind, body) => request(`/api/admin/accounts/${encodeURIComponent(accountID)}/connections/${kind}/verify`, {method: 'POST', body: JSON.stringify(body)}),
  setOptionalConnectionEnabled: (accountID, enabled) => request(`/api/admin/accounts/${encodeURIComponent(accountID)}/connections/apple_account/enabled`, {method:'PATCH', body:JSON.stringify({enabled})}),
  checkConnection: (accountID, kind) => request(`/api/admin/accounts/${encodeURIComponent(accountID)}/connections/${kind}/check`, {method: 'POST'}),
  mailboxes: (params, options = {}) => request('/api/admin/mailboxes?' + new URLSearchParams(params), options),
  messages: params => request('/api/admin/messages?'+new URLSearchParams(params)),
  mailboxOverview: () => request('/api/admin/mailbox-overview'),
  batchMailboxes: (ids,patch) => request('/api/admin/mailboxes/batch',{method:'POST',body:JSON.stringify({ids,patch})}),
  mailboxResult: (id,params,options={}) => request(`/api/admin/mailboxes/${encodeURIComponent(id)}/result?`+new URLSearchParams(params),options),
  job: id => request(`/api/admin/jobs/${encodeURIComponent(id)}`),
  jobItems: id => request(`/api/admin/jobs/${encodeURIComponent(id)}/items`),
  jobEvents: id => request(`/api/admin/jobs/${encodeURIComponent(id)}/events`),
  cancelJob: id => request(`/api/admin/jobs/${encodeURIComponent(id)}/cancel`,{method:'POST'}),
  retryJob: id => request(`/api/admin/jobs/${encodeURIComponent(id)}/retry`,{method:'POST'}),
  createMailbox: body => request('/api/admin/mailboxes', {method: 'POST', body: JSON.stringify(body)}),
  mailbox: id => request(`/api/admin/mailboxes/${encodeURIComponent(id)}`),
  updateMailbox: (id, body) => request(`/api/admin/mailboxes/${encodeURIComponent(id)}`, {method: 'PATCH', body: JSON.stringify(body)}),
  deleteMailbox: id => request(`/api/admin/mailboxes/${encodeURIComponent(id)}`, {method: 'DELETE'}),
  mailboxMessages: (id, page = 1, pageSize = 10) => request(`/api/admin/mailboxes/${encodeURIComponent(id)}/messages?` + new URLSearchParams({page: String(page), page_size: String(pageSize)})),
  mailboxMessage: (mailboxID, messageID) => request(`/api/admin/mailboxes/${encodeURIComponent(mailboxID)}/messages/${encodeURIComponent(messageID)}`),
  syncMailboxMessages: id => request(`/api/admin/mailboxes/${encodeURIComponent(id)}/message-sync-jobs`, {method: 'POST'}),
  rotateMailboxAPI: id => request(`/api/admin/mailboxes/${encodeURIComponent(id)}/api-credential`, {method: 'POST'}),
  previewMailboxSync: body => request('/api/admin/mailbox-sync-previews', {method: 'POST', body: JSON.stringify(body)}),
  confirmMailboxSync: confirmation_token => request('/api/admin/mailbox-sync-jobs', {method: 'POST', body: JSON.stringify({confirmation_token})}),
  jobs: async (params = {}) => (await request('/api/admin/jobs?' + new URLSearchParams({limit: '20', ...params}))) || {items: [], next_cursor: ''},
  createJob: (type, payload = {}) => request('/api/admin/jobs', {method: 'POST', body: JSON.stringify({type, payload})}),
  clearJobs: () => request('/api/admin/jobs', {method: 'DELETE'}),
  clients: async () => (await request('/api/admin/api-clients')) || [],
  createClient: body => request('/api/admin/api-clients', {method: 'POST', body: JSON.stringify(body)}),
  rotateClient: id => request(`/api/admin/api-clients/${encodeURIComponent(id)}/rotate`, {method: 'POST'}),
  setClientStatus: (id, status) => request(`/api/admin/api-clients/${encodeURIComponent(id)}`, {method: 'PATCH', body: JSON.stringify({status})}),
  deleteClient: id => request(`/api/admin/api-clients/${encodeURIComponent(id)}`, {method: 'DELETE'}),
  schedules: async () => (await request('/api/admin/schedules')) || [],
  schedule: id => request(`/api/admin/schedules/${encodeURIComponent(id)}`),
  createSchedule: body => request('/api/admin/schedules', {method: 'POST', body: JSON.stringify(body)}),
  updateSchedule: (id, body) => request(`/api/admin/schedules/${encodeURIComponent(id)}`, {method: 'PATCH', body: JSON.stringify(body)}),
  deleteSchedule: id => request(`/api/admin/schedules/${encodeURIComponent(id)}`, {method: 'DELETE'}),
  runSchedule: id => request(`/api/admin/schedules/${encodeURIComponent(id)}/run`, {method: 'POST'}),
  settings: () => request('/api/admin/settings'),
  saveSettings: body => request('/api/admin/settings', {method: 'PUT', body: JSON.stringify(body)}),
  changePassword: body => request('/api/admin/password', {method: 'PUT', body: JSON.stringify(body)}),
  activity: async (page = 1, pageSize = 20) => (await request('/api/admin/activities?' + new URLSearchParams({page, page_size: pageSize}))) || {items: [], page: 1, page_size: pageSize, total: 0},
  createActivity: body => request('/api/admin/activities', {method: 'POST', body: JSON.stringify(body)}),
  updateActivity: (id, body) => request(`/api/admin/activities/${encodeURIComponent(id)}`, {method: 'PATCH', body: JSON.stringify(body)}),
  deleteActivity: id => request(`/api/admin/activities/${encodeURIComponent(id)}`, {method: 'DELETE'}),
  integrityJob: () => request('/api/admin/jobs', {method: 'POST', body: JSON.stringify({type: 'database.integrity_check', payload: {}})}),
};
