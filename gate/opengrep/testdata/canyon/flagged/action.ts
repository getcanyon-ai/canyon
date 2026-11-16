'use server';
export async function save(formData: FormData) {
  const name = formData.get('name');
  await db.query(`INSERT INTO t VALUES ('${name}')`);
}
