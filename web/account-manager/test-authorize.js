'use strict';
async function submit(approved) {
  document.querySelectorAll('button').forEach(b=>b.disabled=true);
  const q=new URLSearchParams(location.search), notice=document.getElementById('mockNotice');
  try {
    const response=await fetch('/api/enroll-simulate',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({id:q.get('flow'),state:q.get('state'),approved})});
    const job=await response.json();if(!response.ok)throw new Error(job.error);
    let result;
    do {await new Promise(r=>setTimeout(r,200));result=await fetch('/api/jobs/'+job.job).then(r=>r.json());}while(result.pending);
    if(!result.ok)throw new Error(result.error);
    notice.textContent=approved?'已模拟通过。请返回管理台，点击检查授权结果。':'已模拟失败，账号池没有变化。';notice.hidden=false;
  }catch(error){notice.textContent=error.message;notice.classList.add('error');notice.hidden=false;document.querySelectorAll('button').forEach(b=>b.disabled=false);}
}
document.getElementById('approve').addEventListener('click',()=>submit(true));
document.getElementById('reject').addEventListener('click',()=>submit(false));
