const accountId = process.env.CLOUDFLARE_ACCOUNT_ID;
const token = process.env.CLOUDFLARE_API_TOKEN;

if (!accountId || !token) {
  console.error('Cloudflare account ID or API token is missing.');
  process.exit(1);
}

console.log(`Account ID format valid: ${/^[a-f0-9]{32}$/.test(accountId)}`);
console.log(`Account ID has surrounding whitespace: ${accountId !== accountId.trim()}`);
console.log(`API token has surrounding whitespace: ${token !== token.trim()}`);

const checks = [
  ['Token verification', '/user/tokens/verify', (result) => ({ status: result.status, accountIdIsTokenId: result.id === accountId })],
  ['Account details', `/accounts/${encodeURIComponent(accountId)}`, (result) => ({ accountIdMatches: result.id === accountId })],
  ['Workers access', `/accounts/${encodeURIComponent(accountId)}/workers/scripts`, (result) => ({ installerExists: result.some((worker) => worker.id === 'borq-installer') })],
  ['Installer deployments', `/accounts/${encodeURIComponent(accountId)}/workers/scripts/borq-installer/deployments`, () => ({})],
  ['Domain access', '/zones?name=borq.io', (result) => ({ domainVisible: result.length > 0, accountIdMatches: result.some((zone) => zone.account.id === accountId) })],
];

await Promise.all(checks.map(async ([label, path, summarize]) => {
  try {
    const response = await fetch(`https://api.cloudflare.com/client/v4${path}`, {
      headers: { Authorization: `Bearer ${token}` },
      redirect: 'error',
      signal: AbortSignal.timeout(10_000),
    });
    const body = await response.json();
    console.log(JSON.stringify({
      check: label,
      status: response.status,
      success: body.success,
      errorCodes: body.errors?.map((error) => error.code),
      ...(response.ok && body.success ? summarize(body.result) : {}),
    }));
  } catch {
    console.log(JSON.stringify({ check: label, error: 'Request or response processing failed.' }));
  }
}));
