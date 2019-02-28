/*
Copyright IBM Corp All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package token

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/hyperledger/fabric/token/tms/plain"

	"github.com/fsouza/go-dockerclient"
	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric/bccsp/sw"
	"github.com/hyperledger/fabric/integration/nwo"
	"github.com/hyperledger/fabric/msp"
	"github.com/hyperledger/fabric/protos/common"
	"github.com/hyperledger/fabric/protos/token"
	tk "github.com/hyperledger/fabric/token"
	tokenclient "github.com/hyperledger/fabric/token/client"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pkg/errors"
	"github.com/tedsuo/ifrit"
)

var _ bool = Describe("Token EndToEnd", func() {
	var (
		testDir string
		client  *docker.Client
		network *nwo.Network
		process ifrit.Process

		tokensToIssue               []*token.TokenToIssue
		expectedTokenTransaction    *token.TokenTransaction
		expectedUnspentTokens       *token.UnspentTokens
		issuedTokens                []*token.TokenOutput
		expectedTransferTransaction *token.TokenTransaction
		expectedRedeemTransaction   *token.TokenTransaction
		recipientUser1              *token.TokenOwner
		recipientUser2              *token.TokenOwner
	)

	BeforeEach(func() {
		// prepare data for issue tokens
		tokensToIssue = []*token.TokenToIssue{
			{Recipient: &token.TokenOwner{Raw: []byte("test-owner")}, Type: "ABC123", Quantity: ToHex(119)},
		}

		// expected token transaction returned by prover peer
		expectedTokenTransaction = &token.TokenTransaction{
			Action: &token.TokenTransaction_TokenAction{
				TokenAction: &token.TokenAction{
					Data: &token.TokenAction_Issue{
						Issue: &token.Issue{
							Outputs: []*token.Token{{
								Owner:    &token.TokenOwner{Raw: []byte("test-owner")},
								Type:     "ABC123",
								Quantity: ToHex(119),
							}}}}}}}
		expectedTransferTransaction = &token.TokenTransaction{
			Action: &token.TokenTransaction_TokenAction{
				TokenAction: &token.TokenAction{
					Data: &token.TokenAction_Transfer{
						Transfer: &token.Transfer{
							Inputs: nil,
							Outputs: []*token.Token{{
								Owner:    nil,
								Type:     "ABC123",
								Quantity: ToHex(119),
							}},
						},
					},
				},
			},
		}
		expectedRedeemTransaction = &token.TokenTransaction{
			Action: &token.TokenTransaction_TokenAction{
				TokenAction: &token.TokenAction{
					Data: &token.TokenAction_Redeem{
						Redeem: &token.Transfer{
							Inputs: nil,
							Outputs: []*token.Token{
								{
									Owner:    nil,
									Type:     "ABC123",
									Quantity: ToHex(50),
								},
								{
									Owner:    nil,
									Type:     "ABC123",
									Quantity: ToHex(119 - 50),
								},
							},
						},
					},
				},
			},
		}

		expectedUnspentTokens = &token.UnspentTokens{
			Tokens: []*token.TokenOutput{
				{
					Quantity: ToHex(119),
					Type:     "ABC123",
					Id:       &token.TokenId{TxId: "ledger-id", Index: 1},
				},
			},
		}

		var err error
		testDir, err = ioutil.TempDir("", "token-e2e")
		Expect(err).NotTo(HaveOccurred())

		client, err = docker.NewClientFromEnv()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if process != nil {
			process.Signal(syscall.SIGTERM)
			Eventually(process.Wait(), time.Minute).Should(Receive())
		}
		if network != nil {
			network.Cleanup()
		}
		os.RemoveAll(testDir)
	})

	Describe("basic solo network for token transaction e2e using Token CLI", func() {
		BeforeEach(func() {
			var err error
			network = nwo.New(nwo.BasicSoloV20(), testDir, client, 30000, components)
			network.GenerateConfigTree()

			network.Bootstrap()

			client, err = docker.NewClientFromEnv()
			Expect(err).NotTo(HaveOccurred())

			networkRunner := network.NetworkGroupRunner()
			process = ifrit.Invoke(networkRunner)
			Eventually(process.Ready()).Should(BeClosed())

			orderer := network.Orderer("orderer")
			network.CreateAndJoinChannel(orderer, "testchannel")
		})

		It("executes a basic solo network and submits token transaction", func() {
			By("issuing tokens to user2")

			user1, err := getIdentity(network, network.Peer("Org1", "peer1"), "User1", "Org1MSP")
			Expect(err).ToNot(HaveOccurred())
			user2, err := getIdentity(network, network.Peer("Org1", "peer1"), "User2", "Org1MSP")
			Expect(err).ToNot(HaveOccurred())

			// User1 issues 100 ABC123 tokens to User2
			IssueToken(network, network.Peer("Org1", "peer1"), network.Orderer("orderer"),
				"testchannel", "User1", "Org1MSP", "ABC123", "100", string(user2))

			// User2 lists her tokens and verify
			outputs := ListTokens(network, network.Peer("Org1", "peer1"), network.Orderer("orderer"),
				"testchannel", "User2", "Org1MSP")
			Expect(len(outputs)).To(BeEquivalentTo(1))
			fmt.Println(outputs[0].Quantity)
			fmt.Println(ToHex(100))
			Expect(outputs[0].Quantity).To(BeEquivalentTo(ToHex(100)))
			Expect(outputs[0].Type).To(BeEquivalentTo("ABC123"))

			// User2 transfers back the tokens to User1
			TransferTokens(network, network.Peer("Org1", "peer1"), network.Orderer("orderer"),
				"testchannel", "User2", "Org1MSP",
				[]*token.TokenId{outputs[0].Id},
				[]*token.RecipientTransferShare{
					{
						Quantity:  ToHex(50),
						Recipient: &token.TokenOwner{Raw: user2},
					},
					{
						Quantity:  ToHex(50),
						Recipient: &token.TokenOwner{Raw: user1},
					},
				},
			)

			// User2 lists her tokens and verify
			outputs = ListTokens(network, network.Peer("Org1", "peer1"), network.Orderer("orderer"),
				"testchannel", "User2", "Org1MSP")
			Expect(len(outputs)).To(BeEquivalentTo(1))
			Expect(outputs[0].Quantity).To(BeEquivalentTo(ToHex(50)))
			Expect(outputs[0].Type).To(BeEquivalentTo("ABC123"))

			// User1 lists her tokens and verify
			outputs = ListTokens(network, network.Peer("Org1", "peer1"), network.Orderer("orderer"),
				"testchannel", "User2", "Org1MSP")
			Expect(len(outputs)).To(BeEquivalentTo(1))
			Expect(outputs[0].Quantity).To(BeEquivalentTo(ToHex(50)))
			Expect(outputs[0].Type).To(BeEquivalentTo("ABC123"))

			// User1 redeems 25 of her tokens
			RedeemTokens(network, network.Peer("Org1", "peer1"), network.Orderer("orderer"),
				"testchannel", "User2", "Org1MSP",
				[]*token.TokenId{outputs[0].Id}, 25)

			// User1 lists her tokens and verify again
			outputs = ListTokens(network, network.Peer("Org1", "peer1"), network.Orderer("orderer"),
				"testchannel", "User2", "Org1MSP")
			Expect(len(outputs)).To(BeEquivalentTo(1))
			Expect(outputs[0].Quantity).To(BeEquivalentTo(ToHex(25)))
			Expect(outputs[0].Type).To(BeEquivalentTo("ABC123"))
		})

	})

	Describe("basic solo network for token transaction e2e", func() {

		BeforeEach(func() {
			var err error
			network = nwo.New(nwo.BasicSoloV20(), testDir, client, 30000, components)
			network.GenerateConfigTree()

			network.Bootstrap()

			client, err = docker.NewClientFromEnv()
			Expect(err).NotTo(HaveOccurred())

			peer := network.Peer("Org1", "peer1")
			// Get recipients' identity
			recipientUser2Bytes, err := getIdentity(network, peer, "User2", "Org1MSP")
			Expect(err).NotTo(HaveOccurred())
			recipientUser2 = &token.TokenOwner{Raw: recipientUser2Bytes}
			tokensToIssue[0].Recipient = recipientUser2
			expectedTokenTransaction.GetTokenAction().GetIssue().Outputs[0].Owner = recipientUser2
			recipientUser1Bytes, err := getIdentity(network, peer, "User1", "Org1MSP")
			Expect(err).NotTo(HaveOccurred())
			recipientUser1 = &token.TokenOwner{Raw: recipientUser1Bytes}
			expectedTransferTransaction.GetTokenAction().GetTransfer().Outputs[0].Owner = recipientUser1
			expectedRedeemTransaction.GetTokenAction().GetRedeem().Outputs[1].Owner = recipientUser1

			networkRunner := network.NetworkGroupRunner()
			process = ifrit.Invoke(networkRunner)
			Eventually(process.Ready()).Should(BeClosed())
		})

		It("executes a basic solo network and submits token transaction", func() {
			By("getting the orderer by name")
			orderer := network.Orderer("orderer")

			By("setting up the channel")
			network.CreateAndJoinChannel(orderer, "testchannel")

			By("getting the client peer by name")
			peer := network.Peer("Org1", "peer1")

			By("creating a new client")
			config := getClientConfig(network, peer, orderer, "testchannel", "User1", "Org1MSP")
			signingIdentity, err := getSigningIdentity(config.MSPInfo.MSPConfigPath, config.MSPInfo.MSPID, config.MSPInfo.MSPType)
			Expect(err).NotTo(HaveOccurred())
			tClient, err := tokenclient.NewClient(*config, signingIdentity)
			Expect(err).NotTo(HaveOccurred())

			By("issuing tokens to user2")
			txID := RunIssueRequest(tClient, tokensToIssue, expectedTokenTransaction)

			By("list tokens")
			config = getClientConfig(network, peer, orderer, "testchannel", "User2", "Org1MSP")
			signingIdentity, err = getSigningIdentity(config.MSPInfo.MSPConfigPath, config.MSPInfo.MSPID, config.MSPInfo.MSPType)
			Expect(err).NotTo(HaveOccurred())
			tClient, err = tokenclient.NewClient(*config, signingIdentity)
			Expect(err).NotTo(HaveOccurred())
			issuedTokens = RunListTokens(tClient, expectedUnspentTokens)
			Expect(issuedTokens).ToNot(BeNil())
			Expect(len(issuedTokens)).To(Equal(1))

			By("transferring tokens to user1")
			tokenIDs := []*token.TokenId{{TxId: txID, Index: 0}}
			expectedTransferTransaction.GetTokenAction().GetTransfer().Inputs = tokenIDs
			txID = RunTransferRequest(tClient, issuedTokens, recipientUser1, expectedTransferTransaction)

			By("list tokens user 2")
			config = getClientConfig(network, peer, orderer, "testchannel", "User2", "Org1MSP")
			signingIdentity, err = getSigningIdentity(config.MSPInfo.MSPConfigPath, config.MSPInfo.MSPID, config.MSPInfo.MSPType)
			tClient, err = tokenclient.NewClient(*config, signingIdentity)
			Expect(err).NotTo(HaveOccurred())
			issuedTokens = RunListTokens(tClient, nil)

			By("list tokens user 1")
			config = getClientConfig(network, peer, orderer, "testchannel", "User1", "Org1MSP")
			signingIdentity, err = getSigningIdentity(config.MSPInfo.MSPConfigPath, config.MSPInfo.MSPID, config.MSPInfo.MSPType)
			tClient, err = tokenclient.NewClient(*config, signingIdentity)
			Expect(err).NotTo(HaveOccurred())
			issuedTokens = RunListTokens(tClient, expectedUnspentTokens)

			By("redeeming tokens user1")
			tokenIDs = []*token.TokenId{{TxId: txID, Index: 0}}
			expectedRedeemTransaction.GetTokenAction().GetRedeem().Inputs = tokenIDs
			var quantityToRedeem uint64 = 50 // redeem 50 out of 119
			RunRedeemRequest(tClient, issuedTokens, ToHex(quantityToRedeem), expectedRedeemTransaction)

			By("listing tokens user1")
			expectedUnspentTokens = &token.UnspentTokens{
				Tokens: []*token.TokenOutput{
					{
						Quantity: ToHex(119 - 50),
						Type:     "ABC123",
						Id:       &token.TokenId{TxId: "ledger-id", Index: 0},
					},
				},
			}
			issuedTokens = RunListTokens(tClient, expectedUnspentTokens)
		})

	})

	Describe("basic solo network for token transaction bad path e2e ", func() {
		var (
			orderer *nwo.Orderer
			peer    *nwo.Peer
		)

		BeforeEach(func() {
			var err error
			network = nwo.New(nwo.BasicSoloV20(), testDir, client, 30000, components)
			network.GenerateConfigTree()

			network.Bootstrap()

			client, err = docker.NewClientFromEnv()
			Expect(err).NotTo(HaveOccurred())

			peer = network.Peer("Org1", "peer1")

			// Get recipients' identity
			recipientUser2Bytes, err := getIdentity(network, peer, "User2", "Org1MSP")
			Expect(err).NotTo(HaveOccurred())
			recipientUser2 = &token.TokenOwner{Raw: recipientUser2Bytes}
			tokensToIssue[0].Recipient = &token.TokenOwner{Raw: recipientUser2Bytes}
			expectedTokenTransaction.GetTokenAction().GetIssue().Outputs[0].Owner = recipientUser2
			recipientUser1Bytes, err := getIdentity(network, peer, "User1", "Org1MSP")
			Expect(err).NotTo(HaveOccurred())
			recipientUser1 = &token.TokenOwner{Raw: recipientUser1Bytes}
			expectedTransferTransaction.GetTokenAction().GetTransfer().Outputs[0].Owner = &token.TokenOwner{Raw: recipientUser1Bytes}

			networkRunner := network.NetworkGroupRunner()
			process = ifrit.Invoke(networkRunner)
			Eventually(process.Ready()).Should(BeClosed())

			By("getting the orderer by name")
			orderer = network.Orderer("orderer")

			By("setting up the channel")
			network.CreateAndJoinChannel(orderer, "testchannel")
		})

		It("e2e token transfer double spending fails", func() {
			By("User1 issuing tokens to User2")
			tClient := GetTokenClient(network, peer, orderer, "User1", "Org1MSP")
			txID := RunIssueRequest(tClient, tokensToIssue, expectedTokenTransaction)

			By("User2 lists tokens as sanity check")
			tClient = GetTokenClient(network, peer, orderer, "User2", "Org1MSP")
			issuedTokens = RunListTokens(tClient, expectedUnspentTokens)
			Expect(issuedTokens).ToNot(BeNil())
			Expect(len(issuedTokens)).To(Equal(1))

			By("User2 transfers his token to User1")
			inputIDs := []*token.TokenId{{TxId: txID, Index: 0}}
			expectedTransferTransaction.GetTokenAction().GetTransfer().Inputs = inputIDs
			RunTransferRequest(tClient, issuedTokens, recipientUser1, expectedTransferTransaction)

			By("User2 try to transfer again the same token already transferred before")
			txid, ordererStatus, committed, err := RunTransferRequestWithFailure(tClient, issuedTokens, recipientUser1)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(fmt.Sprintf("transaction [%s] status is not valid: INVALID_OTHER_REASON", txid)))
			Expect(*ordererStatus).To(Equal(common.Status_SUCCESS))
			Expect(committed).To(BeFalse())
		})

		It("User1's spending User2's token", func() {
			By("User1 issuing tokens to User2")
			tClient := GetTokenClient(network, peer, orderer, "User1", "Org1MSP")
			txID := RunIssueRequest(tClient, tokensToIssue, expectedTokenTransaction)

			By("User2 lists tokens as sanity check")
			tClient = GetTokenClient(network, peer, orderer, "User2", "Org1MSP")
			issuedTokens = RunListTokens(tClient, expectedUnspentTokens)
			Expect(issuedTokens).ToNot(BeNil())
			Expect(len(issuedTokens)).To(Equal(1))

			By("User1 attempts to spend User2's tokens using the prover peer")
			tClient = GetTokenClient(network, peer, orderer, "User1", "Org1MSP")
			_, ordererStatus, committed, err := RunTransferRequestWithFailure(tClient, issuedTokens, recipientUser1)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError("error from prover: the requestor does not own inputs"))
			Expect(ordererStatus).To(BeNil())
			Expect(committed).To(BeFalse())

			By("User1 attempts to spend User2's tokens by submitting a token transaction directly")
			expectedTokenTransaction = &token.TokenTransaction{
				Action: &token.TokenTransaction_TokenAction{
					TokenAction: &token.TokenAction{
						Data: &token.TokenAction_Transfer{
							Transfer: &token.Transfer{
								Inputs: []*token.TokenId{{TxId: txID, Index: 0}},
								Outputs: []*token.Token{{
									Owner:    &token.TokenOwner{Raw: []byte("test-owner")},
									Type:     "ABC123",
									Quantity: ToHex(119),
								}}}}}}}
			tempTxID, ordererStatus, committed, err := SubmitTokenTx(tClient, expectedTokenTransaction)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(fmt.Sprintf("transaction [%s] status is not valid: INVALID_OTHER_REASON", tempTxID)))
			Expect(*ordererStatus).To(Equal(common.Status_SUCCESS))
			Expect(committed).To(BeFalse())
		})

		It("User1's spending its token to a nil output, to zero value", func() {
			By("User1 issuing tokens to User2")
			tClient := GetTokenClient(network, peer, orderer, "User1", "Org1MSP")
			txID := RunIssueRequest(tClient, tokensToIssue, expectedTokenTransaction)

			By("User2 lists tokens as sanity check")
			tClient = GetTokenClient(network, peer, orderer, "User2", "Org1MSP")
			issuedTokens = RunListTokens(tClient, expectedUnspentTokens)
			Expect(issuedTokens).ToNot(BeNil())
			Expect(len(issuedTokens)).To(Equal(1))

			By("User2 attempts to spend to a nil recipient by using the prover peer")
			tempTxID, ordererStatus, committed, err := RunTransferRequestWithFailure(tClient, issuedTokens, nil)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError("error from prover: invalid recipient in transfer request 'identity cannot be nil'"))
			Expect(ordererStatus).To(BeNil())
			Expect(committed).To(BeFalse())

			By("Check that nothing changed")
			tClient = GetTokenClient(network, peer, orderer, "User2", "Org1MSP")
			issuedTokens = RunListTokens(tClient, expectedUnspentTokens)
			Expect(issuedTokens).ToNot(BeNil())
			Expect(len(issuedTokens)).To(Equal(1))

			By("User2 attempts to spend to a nil recipient by assembling a faulty token transaction")
			expectedTokenTransaction = &token.TokenTransaction{
				Action: &token.TokenTransaction_TokenAction{
					TokenAction: &token.TokenAction{
						Data: &token.TokenAction_Transfer{
							Transfer: &token.Transfer{
								Inputs: []*token.TokenId{{TxId: txID, Index: 0}},
								Outputs: []*token.Token{{
									Owner:    nil,
									Type:     "ABC123",
									Quantity: ToHex(119),
								}}}}}}}
			tempTxID, ordererStatus, committed, err = SubmitTokenTx(tClient, expectedTokenTransaction)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(fmt.Sprintf("transaction [%s] status is not valid: INVALID_OTHER_REASON", tempTxID)))
			Expect(*ordererStatus).To(Equal(common.Status_SUCCESS))
			Expect(committed).To(BeFalse())

			By("Check that nothing changed")
			tClient = GetTokenClient(network, peer, orderer, "User2", "Org1MSP")
			issuedTokens = RunListTokens(tClient, expectedUnspentTokens)
			Expect(issuedTokens).ToNot(BeNil())
			Expect(len(issuedTokens)).To(Equal(1))

			By("User2 attempts to spend to an invalid recipient by using the prover peer")
			tempTxID, ordererStatus, committed, err = RunTransferRequestWithFailure(tClient, issuedTokens, &token.TokenOwner{Raw: []byte{1, 2, 3, 4}})
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError("error from prover: invalid recipient in transfer request 'identity [0x7261773a225c3030315c3030325c3030335c3030342220] cannot be deserialised: could not deserialize a SerializedIdentity: proto: msp.SerializedIdentity: illegal tag 0 (wire type 1)'"))
			Expect(ordererStatus).To(BeNil())
			Expect(committed).To(BeFalse())

			By("Check that nothing changed")
			tClient = GetTokenClient(network, peer, orderer, "User2", "Org1MSP")
			issuedTokens = RunListTokens(tClient, expectedUnspentTokens)
			Expect(issuedTokens).ToNot(BeNil())
			Expect(len(issuedTokens)).To(Equal(1))

			By("User2 attempts to spend to an invalid recipient by assembling a faulty token transaction")
			expectedTokenTransaction = &token.TokenTransaction{
				Action: &token.TokenTransaction_TokenAction{
					TokenAction: &token.TokenAction{
						Data: &token.TokenAction_Transfer{
							Transfer: &token.Transfer{
								Inputs: []*token.TokenId{{TxId: txID, Index: 0}},
								Outputs: []*token.Token{{
									Owner:    &token.TokenOwner{Raw: []byte{1, 2, 3, 4}},
									Type:     "ABC123",
									Quantity: ToHex(119),
								}}}}}}}
			tempTxID, ordererStatus, committed, err = SubmitTokenTx(tClient, expectedTokenTransaction)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(fmt.Sprintf("transaction [%s] status is not valid: INVALID_OTHER_REASON", tempTxID)))
			Expect(*ordererStatus).To(Equal(common.Status_SUCCESS))
			Expect(committed).To(BeFalse())

			By("Check that nothing changed")
			tClient = GetTokenClient(network, peer, orderer, "User2", "Org1MSP")
			issuedTokens = RunListTokens(tClient, expectedUnspentTokens)
			Expect(issuedTokens).ToNot(BeNil())
			Expect(len(issuedTokens)).To(Equal(1))

			By("User2 attempts to transfer zero amount by assembling a faulty token transaction by using the prover peer")
			tempTxID, ordererStatus, committed, err = RunTransferRequestWithSharesAddFailure(tClient, issuedTokens,
				[]*token.RecipientTransferShare{{Recipient: recipientUser1, Quantity: ToHex(0)}})
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError("error from prover: invalid quantity in transfer request 'quantity must be larger than 0'"))
			Expect(ordererStatus).To(BeNil())
			Expect(committed).To(BeFalse())

			By("Check that nothing changed")
			tClient = GetTokenClient(network, peer, orderer, "User2", "Org1MSP")
			issuedTokens = RunListTokens(tClient, expectedUnspentTokens)
			Expect(issuedTokens).ToNot(BeNil())
			Expect(len(issuedTokens)).To(Equal(1))

			By("User2 attempts to transfer zero amount by assembling a faulty token transaction")
			expectedTokenTransaction = &token.TokenTransaction{
				Action: &token.TokenTransaction_TokenAction{
					TokenAction: &token.TokenAction{
						Data: &token.TokenAction_Transfer{
							Transfer: &token.Transfer{
								Inputs: []*token.TokenId{{TxId: txID, Index: 0}},
								Outputs: []*token.Token{{
									Owner:    recipientUser1,
									Type:     "ABC123",
									Quantity: ToHex(0),
								}}}}}}}
			tempTxID, ordererStatus, committed, err = SubmitTokenTx(tClient, expectedTokenTransaction)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(fmt.Sprintf("transaction [%s] status is not valid: INVALID_OTHER_REASON", tempTxID)))
			Expect(*ordererStatus).To(Equal(common.Status_SUCCESS))
			Expect(committed).To(BeFalse())

			By("Check that nothing changed")
			tClient = GetTokenClient(network, peer, orderer, "User2", "Org1MSP")
			issuedTokens = RunListTokens(tClient, expectedUnspentTokens)
			Expect(issuedTokens).ToNot(BeNil())
			Expect(len(issuedTokens)).To(Equal(1))

			By("User2 attempts to transfer an over supported precision amount by assembling a faulty token transaction by using the prover peer")
			tempTxID, ordererStatus, committed, err = RunTransferRequestWithSharesAddFailure(tClient, issuedTokens,
				[]*token.RecipientTransferShare{{Recipient: recipientUser1, Quantity: "99999999999999999999"}})
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError("error from prover: invalid quantity in transfer request '99999999999999999999 has precision 67 > 64'"))
			Expect(ordererStatus).To(BeNil())
			Expect(committed).To(BeFalse())

			By("Check that nothing changed")
			tClient = GetTokenClient(network, peer, orderer, "User2", "Org1MSP")
			issuedTokens = RunListTokens(tClient, expectedUnspentTokens)
			Expect(issuedTokens).ToNot(BeNil())
			Expect(len(issuedTokens)).To(Equal(1))

			By("User2 attempts to transfer an over supported precision amount by assembling a faulty token transaction")
			expectedTokenTransaction = &token.TokenTransaction{
				Action: &token.TokenTransaction_TokenAction{
					TokenAction: &token.TokenAction{
						Data: &token.TokenAction_Transfer{
							Transfer: &token.Transfer{
								Inputs: []*token.TokenId{{TxId: txID, Index: 0}},
								Outputs: []*token.Token{{
									Owner:    recipientUser1,
									Type:     "ABC123",
									Quantity: "99999999999999999999",
								}}}}}}}
			tempTxID, ordererStatus, committed, err = SubmitTokenTx(tClient, expectedTokenTransaction)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(fmt.Sprintf("transaction [%s] status is not valid: INVALID_OTHER_REASON", tempTxID)))
			Expect(*ordererStatus).To(Equal(common.Status_SUCCESS))
			Expect(committed).To(BeFalse())

			By("Check that nothing changed")
			tClient = GetTokenClient(network, peer, orderer, "User2", "Org1MSP")
			issuedTokens = RunListTokens(tClient, expectedUnspentTokens)
			Expect(issuedTokens).ToNot(BeNil())
			Expect(len(issuedTokens)).To(Equal(1))

			By("User2 attempts to transfer a negative amount by assembling a faulty token transaction by using the prover peer")
			tempTxID, ordererStatus, committed, err = RunTransferRequestWithSharesAddFailure(tClient, issuedTokens,
				[]*token.RecipientTransferShare{{Recipient: recipientUser1, Quantity: "-99999999999999999999"}})
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError("error from prover: invalid quantity in transfer request 'quantity must be larger than 0'"))
			Expect(ordererStatus).To(BeNil())
			Expect(committed).To(BeFalse())

			By("Check that nothing changed")
			tClient = GetTokenClient(network, peer, orderer, "User2", "Org1MSP")
			issuedTokens = RunListTokens(tClient, expectedUnspentTokens)
			Expect(issuedTokens).ToNot(BeNil())
			Expect(len(issuedTokens)).To(Equal(1))

			By("User2 attempts to transfer a negative amount by assembling a faulty token transaction")
			expectedTokenTransaction = &token.TokenTransaction{
				Action: &token.TokenTransaction_TokenAction{
					TokenAction: &token.TokenAction{
						Data: &token.TokenAction_Transfer{
							Transfer: &token.Transfer{
								Inputs: []*token.TokenId{{TxId: txID, Index: 0}},
								Outputs: []*token.Token{{
									Owner:    recipientUser1,
									Type:     "ABC123",
									Quantity: "-99999999999999999999",
								}}}}}}}
			tempTxID, ordererStatus, committed, err = SubmitTokenTx(tClient, expectedTokenTransaction)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(fmt.Sprintf("transaction [%s] status is not valid: INVALID_OTHER_REASON", tempTxID)))
			Expect(*ordererStatus).To(Equal(common.Status_SUCCESS))
			Expect(committed).To(BeFalse())

			By("Check that nothing changed")
			tClient = GetTokenClient(network, peer, orderer, "User2", "Org1MSP")
			issuedTokens = RunListTokens(tClient, expectedUnspentTokens)
			Expect(issuedTokens).ToNot(BeNil())
			Expect(len(issuedTokens)).To(Equal(1))
		})
	})

})

func GetTokenClient(network *nwo.Network, peer *nwo.Peer, orderer *nwo.Orderer, user, mspID string) *tokenclient.Client {
	config := getClientConfig(network, peer, orderer, "testchannel", user, mspID)
	signingIdentity, err := getSigningIdentity(config.MSPInfo.MSPConfigPath, config.MSPInfo.MSPID, config.MSPInfo.MSPType)
	Expect(err).NotTo(HaveOccurred())
	tClient, err := tokenclient.NewClient(*config, signingIdentity)
	Expect(err).NotTo(HaveOccurred())

	return tClient
}

func RunIssueRequest(c *tokenclient.Client, tokens []*token.TokenToIssue, expectedTokenTx *token.TokenTransaction) string {
	envelope, txid, ordererStatus, committed, err := c.Issue(tokens, 30*time.Second)
	Expect(err).NotTo(HaveOccurred())
	Expect(*ordererStatus).To(Equal(common.Status_SUCCESS))
	Expect(committed).To(Equal(true))

	payload := common.Payload{}
	err = proto.Unmarshal(envelope.Payload, &payload)

	tokenTx := &token.TokenTransaction{}
	err = proto.Unmarshal(payload.Data, tokenTx)
	Expect(proto.Equal(tokenTx, expectedTokenTx)).To(BeTrue())

	tokenTxid, err := tokenclient.GetTransactionID(envelope)
	Expect(err).NotTo(HaveOccurred())
	Expect(tokenTxid).To(Equal(txid))
	return txid
}

func RunListTokens(c *tokenclient.Client, expectedUnspentTokens *token.UnspentTokens) []*token.TokenOutput {
	tokenOutputs, err := c.ListTokens()
	Expect(err).NotTo(HaveOccurred())

	// unmarshall CommandResponse and verify the result
	if expectedUnspentTokens == nil {
		Expect(len(tokenOutputs)).To(Equal(0))
	} else {
		Expect(len(tokenOutputs)).To(Equal(len(expectedUnspentTokens.Tokens)))
		for i, token := range expectedUnspentTokens.Tokens {
			Expect(tokenOutputs[i].Type).To(Equal(token.Type))
			Expect(tokenOutputs[i].Quantity).To(Equal(token.Quantity))
		}
	}
	return tokenOutputs
}

func RunTransferRequest(c *tokenclient.Client, inputTokens []*token.TokenOutput, recipient *token.TokenOwner, expectedTokenTx *token.TokenTransaction) string {
	inputTokenIDs := make([]*token.TokenId, len(inputTokens))
	for i, inToken := range inputTokens {
		inputTokenIDs[i] = &token.TokenId{TxId: inToken.GetId().TxId, Index: inToken.GetId().Index}
	}
	shares := []*token.RecipientTransferShare{
		{Recipient: recipient, Quantity: ToHex(119)},
	}

	envelope, txid, ordererStatus, committed, err := c.Transfer(inputTokenIDs, shares, 30*time.Second)
	Expect(err).NotTo(HaveOccurred())
	Expect(*ordererStatus).To(Equal(common.Status_SUCCESS))
	Expect(committed).To(Equal(true))

	payload := common.Payload{}
	err = proto.Unmarshal(envelope.Payload, &payload)

	tokenTx := &token.TokenTransaction{}
	err = proto.Unmarshal(payload.Data, tokenTx)
	By(tokenTx.String())
	By(expectedTokenTx.String())

	Expect(proto.Equal(tokenTx, expectedTokenTx)).To(BeTrue())

	tokenTxid, err := tokenclient.GetTransactionID(envelope)
	Expect(err).NotTo(HaveOccurred())
	Expect(tokenTxid).To(Equal(txid))

	// Verify the result
	Expect(tokenTx.GetTokenAction().GetTransfer().GetInputs()).ToNot(BeNil())
	Expect(len(tokenTx.GetTokenAction().GetTransfer().GetInputs())).To(Equal(1))
	Expect(tokenTx.GetTokenAction().GetTransfer().GetOutputs()).ToNot(BeNil())

	return txid
}

func RunRedeemRequest(c *tokenclient.Client, inputTokens []*token.TokenOutput, quantity string, expectedTokenTx *token.TokenTransaction) {
	inputTokenIDs := make([]*token.TokenId, len(inputTokens))
	for i, inToken := range inputTokens {
		inputTokenIDs[i] = &token.TokenId{TxId: inToken.GetId().TxId, Index: inToken.GetId().Index}
	}

	envelope, txid, ordererStatus, committed, err := c.Redeem(inputTokenIDs, quantity, 30*time.Second)
	Expect(err).NotTo(HaveOccurred())
	Expect(*ordererStatus).To(Equal(common.Status_SUCCESS))
	Expect(committed).To(Equal(true))

	payload := common.Payload{}
	err = proto.Unmarshal(envelope.Payload, &payload)

	tokenTx := &token.TokenTransaction{}
	err = proto.Unmarshal(payload.Data, tokenTx)
	Expect(proto.Equal(tokenTx, expectedTokenTx)).To(BeTrue())

	tokenTxid, err := tokenclient.GetTransactionID(envelope)
	Expect(err).NotTo(HaveOccurred())
	Expect(tokenTxid).To(Equal(txid))
}

func RunTransferRequestWithFailure(c *tokenclient.Client, inputTokens []*token.TokenOutput, recipient *token.TokenOwner) (string, *common.Status, bool, error) {
	inputTokenIDs := make([]*token.TokenId, len(inputTokens))
	sum := plain.NewZeroQuantity(64)
	var err error
	for i, inToken := range inputTokens {
		inputTokenIDs[i] = &token.TokenId{TxId: inToken.GetId().TxId, Index: inToken.GetId().Index}

		v, err := plain.ToQuantity(inToken.GetQuantity(), 64)
		Expect(err).NotTo(HaveOccurred())
		sum, err = sum.Add(v)
		Expect(err).NotTo(HaveOccurred())
	}
	shares := []*token.RecipientTransferShare{
		{Recipient: recipient, Quantity: sum.Hex()},
	}

	_, txid, ordererStatus, committed, err := c.Transfer(inputTokenIDs, shares, 30*time.Second)
	return txid, ordererStatus, committed, err
}

func RunTransferRequestWithSharesAddFailure(c *tokenclient.Client, inputTokens []*token.TokenOutput, shares []*token.RecipientTransferShare) (string, *common.Status, bool, error) {
	inputTokenIDs := make([]*token.TokenId, len(inputTokens))
	sum := plain.NewZeroQuantity(64)
	var err error
	for i, inToken := range inputTokens {
		inputTokenIDs[i] = &token.TokenId{TxId: inToken.GetId().TxId, Index: inToken.GetId().Index}

		v, err := plain.ToQuantity(inToken.GetQuantity(), 64)
		Expect(err).NotTo(HaveOccurred())
		sum, err = sum.Add(v)
		Expect(err).NotTo(HaveOccurred())
	}
	_, txid, ordererStatus, committed, err := c.Transfer(inputTokenIDs, shares, 30*time.Second)
	return txid, ordererStatus, committed, err
}

func SubmitTokenTx(c *tokenclient.Client, tokenTx *token.TokenTransaction) (string, *common.Status, bool, error) {
	serializedTokenTx, err := proto.Marshal(tokenTx)
	Expect(err).ToNot(HaveOccurred())

	txEnvelope, txid, err := c.TxSubmitter.CreateTxEnvelope(serializedTokenTx)
	Expect(err).ToNot(HaveOccurred())

	ordererStatus, committed, err := c.TxSubmitter.Submit(txEnvelope, 30*time.Second)
	return txid, ordererStatus, committed, err
}

func getIdentity(n *nwo.Network, peer *nwo.Peer, user, mspId string) ([]byte, error) {
	orderer := n.Orderer("orderer")
	config := getClientConfig(n, peer, orderer, "testchannel", user, mspId)

	localMSP, err := LoadLocalMSPAt(config.MSPInfo.MSPConfigPath, config.MSPInfo.MSPID, config.MSPInfo.MSPType)
	if err != nil {
		return nil, err
	}

	signer, err := localMSP.GetDefaultSigningIdentity()
	if err != nil {
		return nil, err
	}

	creator, err := signer.Serialize()
	if err != nil {
		return nil, err
	}

	return creator, nil
}

func getSigningIdentity(mspConfigPath, mspID, mspType string) (tk.SigningIdentity, error) {
	mspInstance, err := LoadLocalMSPAt(mspConfigPath, mspID, mspType)
	if err != nil {
		return nil, err
	}

	signingIdentity, err := mspInstance.GetDefaultSigningIdentity()
	if err != nil {
		return nil, err
	}

	return signingIdentity, nil
}

// LoadLocalMSPAt loads an MSP whose configuration is stored at 'dir', and whose
// id and type are the passed as arguments.
func LoadLocalMSPAt(dir, id, mspType string) (msp.MSP, error) {
	if mspType != "bccsp" {
		return nil, errors.Errorf("invalid msp type, expected 'bccsp', got %s", mspType)
	}
	conf, err := msp.GetLocalMspConfig(dir, nil, id)
	if err != nil {
		return nil, err
	}
	ks, err := sw.NewFileBasedKeyStore(nil, filepath.Join(dir, "keystore"), true)
	if err != nil {
		return nil, err
	}
	thisMSP, err := msp.NewBccspMspWithKeyStore(msp.MSPv1_0, ks)
	if err != nil {
		return nil, err
	}
	err = thisMSP.Setup(conf)
	if err != nil {
		return nil, err
	}
	return thisMSP, nil
}

// createCompositeKey and its related functions and consts copied from core/chaincode/shim/chaincode.go
func createCompositeKey(objectType string, attributes []string) (string, error) {
	if err := validateCompositeKeyAttribute(objectType); err != nil {
		return "", err
	}
	ck := compositeKeyNamespace + objectType + string(minUnicodeRuneValue)
	for _, att := range attributes {
		if err := validateCompositeKeyAttribute(att); err != nil {
			return "", err
		}
		ck += att + string(minUnicodeRuneValue)
	}
	return ck, nil
}

func validateCompositeKeyAttribute(str string) error {
	if !utf8.ValidString(str) {
		return errors.Errorf("not a valid utf8 string: [%x]", str)
	}
	for index, runeValue := range str {
		if runeValue == minUnicodeRuneValue || runeValue == maxUnicodeRuneValue {
			return errors.Errorf(`input contain unicode %#U starting at position [%d]. %#U and %#U are not allowed in the input attribute of a composite key`,
				runeValue, index, minUnicodeRuneValue, maxUnicodeRuneValue)
		}
	}
	return nil
}

const (
	minUnicodeRuneValue   = 0            //U+0000
	maxUnicodeRuneValue   = utf8.MaxRune //U+10FFFF - maximum (and unallocated) code point
	compositeKeyNamespace = "\x00"
)
