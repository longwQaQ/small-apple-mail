import {request, setCSRF} from './api.js?v=1.0.0';
import {setButtonBusy} from './components.js?v=1.0.2';

const form = document.querySelector('#login-form');
const username = document.querySelector('#username');
const password = document.querySelector('#password');
const errorBox = document.querySelector('#login-error');

function showError(message) { errorBox.textContent = message; errorBox.hidden = false; errorBox.focus(); }
function clearErrors() { errorBox.hidden = true; errorBox.textContent = ''; document.querySelectorAll('.field-error').forEach(n => n.textContent = ''); }
function validate() {
  clearErrors(); let ok = true;
  if (username.value.trim().length < 3) { document.querySelector('[data-error-for=username]').textContent = '请输入有效的管理员账号'; ok = false; }
  if (Array.from(password.value).length < 8) { document.querySelector('[data-error-for=password]').textContent = '管理员密码至少需要 8 个字符'; ok = false; }
  return ok;
}

form.addEventListener('submit', async event => {
  event.preventDefault(); if (!validate()) return;
  const button = document.querySelector('#login-submit'); setButtonBusy(button, true, '正在验证');
  try {
    const data = await request('/api/admin/session', {method:'POST', body:JSON.stringify({username:username.value.trim(),password:password.value})});
    setCSRF(data.csrf_token); location.replace('/#dashboard');
  } catch (error) { password.value = ''; showError(error.message + (error.requestID ? `（请求 ${error.requestID}）` : '')); }
  finally { setButtonBusy(button, false, '进入控制台'); }
});

document.addEventListener('click', event => { if (!event.target.closest('[data-action=toggle-password]')) return; const visible = password.type === 'text'; password.type = visible ? 'password' : 'text'; event.target.closest('button').setAttribute('aria-label', visible ? '显示密码' : '隐藏密码'); });
password.addEventListener('keyup', event => { document.querySelector('#caps-lock').hidden = !event.getModifierState('CapsLock'); });

request('/api/admin/session').then(() => location.replace('/#dashboard')).catch(() => {});
