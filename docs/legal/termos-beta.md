# Termos de uso do beta da Otter Labs Cloud

> **Rascunho para revisão (#125).** Os campos `{{…}}` são preenchidos pelo
> portal ao publicar, a partir da configuração do servidor; veja
> [README.md](README.md). A versão publicada no portal é a que vale; esta é
> a fonte dela.

Última atualização: {{data_publicacao}}.

## 1. Quem oferece o serviço

A Otter Labs Cloud é um projeto pessoal de {{controlador}}, pessoa física
("nós", "o responsável"). "Otter Labs" é o nome do projeto, não uma
empresa. Contato para tudo o que estes termos tratam: {{contato}}.

## 2. O que é o beta

- Bancos de dados PostgreSQL gerenciados, criados pelo portal, em clusters
  compartilhados com outros clientes. Você recebe um banco e um usuário que
  é dono dele, sem acesso de superusuário nem ao servidor.
- **Gratuito e por convite.** O beta não tem cobrança. Os limites de cada
  conta (quantidade e tamanho de bancos) aparecem no portal e podem mudar.
- **É um teste.** O serviço pode mudar, ficar indisponível ou acabar. Se o
  beta for encerrado, avisaremos por e-mail com pelo menos 30 dias de
  antecedência, para que você exporte seus dados (`pg_dump`), salvo quando
  a lei ou um incidente de segurança exigir antes.

## 3. Sem garantia de disponibilidade nem de desempenho

- Não há acordo de nível de serviço (SLA). O serviço pode parar para
  manutenção, atualizações de segurança ou por falhas, com ou sem aviso.
- Não prometemos vazão nem tempo de resposta. A capacidade de rede é
  compartilhada entre todos os clientes: consultas que devolvem muitos
  dados, ou exportações grandes, podem ser lentas.
- **Não use o beta como única cópia de dados importantes.** Faça suas
  próprias cópias com `pg_dump`.

## 4. Cópias de segurança

Fazemos cópias para nos recuperar de falhas, não como um serviço de backup
para você:

- uma cópia lógica (`pg_dump`) de cada banco por dia, mantida 7 dias, e uma
  por semana, mantida 4 semanas, cifradas antes de sair do servidor;
- cópias do servidor inteiro (snapshots), que são usadas para recuperar um
  cluster todo, nunca para voltar o banco de um único cliente.

Podemos, **a pedido e sem garantia**, recuperar uma cópia do seu banco em um
banco separado, de onde você leva o que precisar. Não substituímos o seu
banco em uso por uma cópia antiga.

## 5. Uso aceitável

Você é responsável pelo que guarda e faz com o seu banco. **Não é
permitido:**

1. guardar ou processar conteúdo ilegal, ou usar o serviço para atividade
   ilegal;
2. guardar **dados pessoais sensíveis** (art. 5º, II, da LGPD: origem racial
   ou étnica, saúde, vida sexual, dados genéticos ou biométricos, convicção
   religiosa, opinião política, filiação sindical), nem dados de crianças e
   adolescentes. O beta não oferece as garantias que esses dados exigem;
3. tentar acessar dados, bancos, contas ou sistemas que não são seus, ou
   contornar o isolamento entre clientes, os limites da conta ou os
   controles de acesso;
4. testar a segurança do serviço (varreduras, testes de invasão) sem
   autorização por escrito. Se encontrar uma falha, escreva para
   {{contato}}: agradecemos e não tomaremos medidas contra quem
   reportar de boa-fé;
5. usar o serviço para mineração de criptomoedas, envio de spam, ataques de
   negação de serviço, ou cargas que prejudiquem de propósito os outros
   clientes;
6. compartilhar sua conta ou revender o acesso.

## 6. Suspensão e encerramento

- Podemos suspender um banco ou uma conta que viole estes termos ou ponha
  em risco o serviço ou outros clientes. Em um incidente de segurança, a
  suspensão pode ser imediata; nos demais casos, avisaremos antes, quando
  possível.
- Você pode excluir seus bancos a qualquer momento pelo portal, e pedir a
  exclusão da sua conta por e-mail.
- Ao excluir um banco, ele deixa de ser acessível na hora. A última cópia
  lógica é mantida por 3 dias (7, se o banco expirou por prazo) e depois
  apagada, assim como as cópias diárias e semanais. Os snapshots do servidor
  (seção 4) ainda contêm o banco até expirarem: por até 13 meses.

## 7. Seus dados e os dados dos seus usuários

- **O conteúdo dos seus bancos é seu.** Em relação aos dados pessoais que
  você guarda neles, você é o controlador e nós somos operador (LGPD, art.
  5º, VI e VII): tratamos esses dados apenas para prestar o serviço
  (armazenar, fazer cópias, recuperar a pedido).
- Não lemos o conteúdo dos seus bancos, exceto para recuperar uma cópia a
  seu pedido, investigar um incidente de segurança ou cumprir ordem
  judicial ou obrigação legal.
- Os dados da sua conta (e-mail, registros de acesso) são tratados conforme
  o [Aviso de privacidade](privacidade.md).

## 8. Responsabilidade

O beta é oferecido gratuitamente, no estado em que se encontra. Na máxima
extensão permitida pela lei, não respondemos por perda de dados,
indisponibilidade ou lucros cessantes decorrentes do uso do beta. Nada
nestes termos afasta direitos que a lei brasileira não permite afastar.

## 9. Alterações

Podemos alterar estes termos. Avisaremos por e-mail com pelo menos 15 dias
de antecedência das mudanças que afetem seus direitos; continuar usando o
serviço depois disso é aceitar a nova versão.

## 10. Lei aplicável

Estes termos seguem a legislação brasileira.
