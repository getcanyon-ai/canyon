export default async function Page(props: { searchParams: Promise<{ q: string }> }) {
  const { q } = await props.searchParams;
  const rows = await query(`SELECT * FROM t WHERE q = '${q}'`);
  return <div>{rows.length}</div>;
}
