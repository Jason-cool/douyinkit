package douyinkit

// Locate only the extra identity-verification panel. Ordinary SMS login and
// customer messages are deliberately excluded. Never select “发送短信验证”.
const smsVerificationDOMJS = `() => {
  const visible = e => {
    if (!e) return false;
    const r = e.getBoundingClientRect();
    if (r.width < 2 || r.height < 2) return false;
    for (let p=e; p && p.nodeType===1; p=p.parentElement) {
      const s=getComputedStyle(p);
      if (s.display==='none' || s.visibility==='hidden' || +s.opacity===0) return false;
    }
    return true;
  };
  const text = e => (e.innerText || '').trim();
  const disabled = e => !e || e.disabled || !!e.closest('[disabled],[aria-disabled="true"]') ||
    e.hasAttribute('disabled') || getComputedStyle(e).pointerEvents==='none';
  const find = (root, test) => [...root.querySelectorAll('button,a,div,span')]
    .filter(e => visible(e) && test(text(e))).sort((a,b) => a.querySelectorAll('*').length-b.querySelectorAll('*').length)[0];
  const headings=[...document.querySelectorAll('h1,h2,h3,h4,div,span')]
    .filter(e => visible(e) && ['身份验证','接收短信验证码'].includes(text(e)));
  for (const heading of headings) {
    for (let root=heading.parentElement,i=0;root && i<7;root=root.parentElement,i++) {
      const t=text(root);
      if (t.length>1800) break;
      const input=[...root.querySelectorAll('input')].find(e => visible(e) &&
        (/验证码/.test(e.placeholder || '') || e.autocomplete==='one-time-code'));
      const isSMS=text(heading)==='接收短信验证码' && input && /验证/.test(t);
      const isChoice=text(heading)==='身份验证' && /为保障账号安全.*请先完成身份验证/.test(t) &&
        /接收短信验证码|发送短信验证/.test(t);
      if (!isSMS && !isChoice) continue;
      const receive=find(root,s=>s==='接收短信验证码');
      const resend=find(root,s=>/^(重新发送|重新获取|获取验证码|发送验证码)$/.test(s));
      const submit=find(root,s=>s==='验证');
      const retry=t.match(/(\d{1,3})\s*(?:s|秒)\s*(?:后)?\s*(?:重新发送|重新获取|重发)/i);
      const masked=t.match(/1\d{2}[*＊•·]{3,8}\d{2,4}/);
      const raw=t.match(/(?:已发送至|已发送到)\s*(1\d{10})/);
      const phone=masked ? masked[0] : raw ? raw[1].slice(0,3)+'****'+raw[1].slice(-4) : '';
      const error=t.match(/错误次数过多(?:或验证码过期)?(?:[，,]请稍后重试)?|验证码(?:错误|不正确|无效|(?:已)?过期|已失效)|请求过于频繁|操作(?:过于)?频繁|发送失败|验证失败|(?:验证|发送)次数[^\n]{0,12}上限/);
      const max=input && input.maxLength;
      return {root,input,receive,resend,submit,step:isSMS?'code_sent':'choose_method',phone,
        retry:retry?+retry[1]:0,canResend:isSMS && !!resend && !disabled(resend) && !retry,
        canSubmit:isSMS && !!input && !disabled(input),codeLength:max>=4 && max<=8?max:6,
        error:error?error[0]:''};
    }
  }
  return null;
}`

const smsVerificationStateJS = `() => {
  const d=(` + smsVerificationDOMJS + `)();
  if (!d) return null;
  return {method:'receive_sms',step:d.step,phone_masked:d.phone,retry_after_seconds:d.retry,
    can_resend:d.canResend,can_submit:d.canSubmit,code_length:d.codeLength,error:d.error};
}`

const markSMSActionJS = `(action) => {
  const d=(` + smsVerificationDOMJS + `)();
  if (!d) return false;
  document.querySelectorAll('[data-dy-sms-action]').forEach(e=>e.removeAttribute('data-dy-sms-action'));
  const el=action==='receive'?d.receive:action==='resend'?d.resend:action==='input'?d.input:action==='submit'?d.submit:null;
  if (!el) return false;
  el.setAttribute('data-dy-sms-action',action);
  return true;
}`
